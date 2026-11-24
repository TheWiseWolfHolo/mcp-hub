package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	defaultPeekMaxBytes = 16 << 10 // 16 KiB
	maxTrackedTools     = 512
	maxTrackedSessions  = 256
	maxResponseTapBytes = 8 << 10 // 8 KiB
	maxStrictJSONDepth  = 256
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// MCPToolCallRef identifies a single tools/call invocation inside a JSON-RPC request or batch.
type MCPToolCallRef struct {
	ID   json.RawMessage
	Name string
}

// MCPPeekInfo summarizes JSON-RPC metadata extracted non-destructively from an HTTP POST request.
// It intentionally never retains or logs params.arguments.
type MCPPeekInfo struct {
	IsJSONRPC    bool
	Method       string
	ID           json.RawMessage
	ToolName     string
	IsBatch      bool
	HasToolsList bool
	HasToolsCall bool
	ToolCalls    []MCPToolCallRef
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}

// PeekMCPRequest non-destructively reads up to maxBytes (default 16 KiB) of a JSON POST request body,
// restores r.Body so downstream handlers read the exact unmodified payload, and extracts JSON-RPC
// method, ID, and tools/call names (for both single objects and batches, tolerating truncation).
func PeekMCPRequest(r *http.Request, maxBytes int64) (MCPPeekInfo, error) {
	if r == nil || r.Method != http.MethodPost || r.Body == nil {
		return MCPPeekInfo{}, nil
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return MCPPeekInfo{}, nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultPeekMaxBytes
	}

	origBody := r.Body
	head, readErr := io.ReadAll(io.LimitReader(origBody, maxBytes))
	r.Body = multiReadCloser{
		Reader: io.MultiReader(bytes.NewReader(head), origBody),
		Closer: origBody,
	}
	if readErr != nil {
		return MCPPeekInfo{}, readErr
	}
	return parseMCPPeekBytes(head), nil
}

// StrictMCPItem represents one strictly validated JSON-RPC 2.0 object in a single request or batch.
type StrictMCPItem struct {
	Raw       []byte
	JSONRPC   string
	HasID     bool
	ID        json.RawMessage
	Method    string
	HasMethod bool
	HasResult bool
	HasError  bool
	ToolName  string // Guaranteed non-empty when Method == "tools/call".
}

// StrictMCPRequest holds the strictly validated JSON-RPC 2.0 payload and derived MCPPeekInfo.
type StrictMCPRequest struct {
	IsBatch bool
	Items   []StrictMCPItem
	Peek    MCPPeekInfo
}

type strictRPCError struct {
	id   json.RawMessage
	code int
	msg  string
}

func (e *strictRPCError) Error() string { return e.msg }

func extractStrictRPCErrorID(err error) json.RawMessage {
	var se *strictRPCError
	if errors.As(err, &se) && len(se.id) > 0 {
		return cloneRawMessage(se.id)
	}
	return json.RawMessage("null")
}

// ValidateStrictMCPHeaders enforces strict JSON media type and rejects Content-Encoding
// when ToolFilter is active so compressed or ambiguously typed payloads cannot bypass inspection.
func ValidateStrictMCPHeaders(r *http.Request) (int, string, error) {
	if r == nil {
		return http.StatusBadRequest, "invalid_request", errors.New("nil request")
	}
	if strings.TrimSpace(r.Header.Get("Content-Encoding")) != "" {
		return http.StatusUnsupportedMediaType, "unsupported_content_encoding", errors.New("Content-Encoding is not supported when tool filtering is active")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return http.StatusUnsupportedMediaType, "unsupported_media_type", errors.New("Content-Type must be application/json")
	}
	return 0, "", nil
}

// ParseStrictMCPRequest strictly parses a complete JSON-RPC 2.0 request object or batch array
// using token scanning (never case-insensitive struct unmarshaling).
// It fails closed on:
//   - empty/malformed JSON or trailing data
//   - empty batch arrays or non-object batch elements
//   - duplicate keys in ANY JSON object at any nesting depth
//   - case-variant envelope keys (jsonrpc/id/method/params/result/error) or params key (name)
//   - missing or non-"2.0" jsonrpc version
//   - objects missing method (unless a valid client response with id + result/error)
//   - tools/call without an object params containing a non-empty string "name"
func ParseStrictMCPRequest(body []byte) (StrictMCPRequest, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(body, utf8BOM))
	if len(trimmed) == 0 {
		return StrictMCPRequest{}, &strictRPCError{code: -32600, msg: "empty JSON-RPC body"}
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return StrictMCPRequest{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid JSON: %v", err)}
	}

	var out StrictMCPRequest
	delim, ok := tok.(json.Delim)
	if !ok {
		return StrictMCPRequest{}, &strictRPCError{code: -32600, msg: "JSON-RPC payload must be an object or batch array"}
	}

	switch delim {
	case '{':
		item, err := parseStrictRPCObject(dec)
		if err != nil {
			return StrictMCPRequest{}, err
		}
		item.Raw = trimmed
		out.Items = []StrictMCPItem{item}

	case '[':
		out.IsBatch = true
		for dec.More() {
			elemTok, err := dec.Token()
			if err != nil {
				return StrictMCPRequest{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid JSON in batch: %v", err)}
			}
			elemDelim, ok := elemTok.(json.Delim)
			if !ok || elemDelim != '{' {
				return StrictMCPRequest{}, &strictRPCError{code: -32600, msg: "batch elements must be JSON-RPC objects"}
			}
			item, err := parseStrictRPCObject(dec)
			if err != nil {
				return StrictMCPRequest{}, err
			}
			out.Items = append(out.Items, item)
		}
		endTok, err := dec.Token()
		if err != nil || endTok != json.Delim(']') {
			return StrictMCPRequest{}, &strictRPCError{code: -32700, msg: "unterminated JSON batch array"}
		}
		if len(out.Items) == 0 {
			return StrictMCPRequest{}, &strictRPCError{code: -32600, msg: "empty JSON-RPC batch"}
		}

	default:
		return StrictMCPRequest{}, &strictRPCError{code: -32600, msg: "JSON-RPC payload must be an object or batch array"}
	}

	// Reject any trailing tokens/garbage after the root JSON value.
	if extraTok, err := dec.Token(); err != io.EOF {
		return StrictMCPRequest{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("unexpected trailing data after JSON-RPC payload (%v)", extraTok)}
	}

	// For valid batches, capture exact per-element raw JSON slices (safe now that strict scan succeeded).
	if out.IsBatch {
		var rawElems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawElems); err != nil || len(rawElems) != len(out.Items) {
			return StrictMCPRequest{}, &strictRPCError{code: -32700, msg: "failed to split batch elements"}
		}
		for i := range out.Items {
			out.Items[i].Raw = bytes.TrimSpace(rawElems[i])
		}
	}

	// Build derived MCPPeekInfo from strictly validated items.
	info := MCPPeekInfo{
		IsJSONRPC: true,
		IsBatch:   out.IsBatch,
	}
	for _, item := range out.Items {
		if info.Method == "" && item.Method != "" {
			info.Method = item.Method
		}
		if len(info.ID) == 0 && len(item.ID) > 0 {
			info.ID = cloneRawMessage(item.ID)
		}
		if item.Method == "tools/list" {
			info.HasToolsList = true
		}
		if item.Method == "tools/call" {
			info.HasToolsCall = true
			if info.ToolName == "" {
				info.ToolName = item.ToolName
			}
			info.ToolCalls = append(info.ToolCalls, MCPToolCallRef{
				ID:   cloneRawMessage(item.ID),
				Name: item.ToolName,
			})
		}
	}
	out.Peek = info
	return out, nil
}

func parseStrictRPCObject(dec *json.Decoder) (StrictMCPItem, error) {
	seen := make(map[string]struct{}, 8)
	var (
		item            StrictMCPItem
		hasJSONRPC      bool
		paramsIsObject  bool
		paramsHasName   bool
		paramsName      string
		paramsNameError string
	)

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return StrictMCPItem{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid JSON object key: %v", err)}
		}
		key, ok := keyTok.(string)
		if !ok {
			return StrictMCPItem{}, &strictRPCError{code: -32700, msg: "JSON object key must be string"}
		}
		if _, dup := seen[key]; dup {
			return StrictMCPItem{}, &strictRPCError{code: -32600, msg: fmt.Sprintf("duplicate JSON key %q", key)}
		}
		seen[key] = struct{}{}

		for _, reserved := range [...]string{"jsonrpc", "id", "method", "params", "result", "error"} {
			if key != reserved && strings.EqualFold(key, reserved) {
				return StrictMCPItem{}, &strictRPCError{code: -32600, msg: fmt.Sprintf("non-canonical JSON-RPC key %q", key)}
			}
		}

		switch key {
		case "jsonrpc":
			vTok, err := dec.Token()
			if err != nil {
				return StrictMCPItem{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid jsonrpc value: %v", err)}
			}
			s, ok := vTok.(string)
			if !ok {
				if err := skipStrictAfterToken(dec, vTok, 1); err != nil {
					return StrictMCPItem{}, err
				}
				return StrictMCPItem{}, &strictRPCError{code: -32600, msg: "jsonrpc must be string \"2.0\""}
			}
			hasJSONRPC = true
			item.JSONRPC = s

		case "method":
			vTok, err := dec.Token()
			if err != nil {
				return StrictMCPItem{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid method value: %v", err)}
			}
			s, ok := vTok.(string)
			if !ok {
				if err := skipStrictAfterToken(dec, vTok, 1); err != nil {
					return StrictMCPItem{}, err
				}
				return StrictMCPItem{}, &strictRPCError{code: -32600, msg: "method must be a string"}
			}
			item.HasMethod = true
			item.Method = s

		case "id":
			vTok, err := dec.Token()
			if err != nil {
				return StrictMCPItem{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid id value: %v", err)}
			}
			idRaw, err := encodeStrictIDToken(vTok)
			if err != nil {
				if skipErr := skipStrictAfterToken(dec, vTok, 1); skipErr != nil {
					return StrictMCPItem{}, skipErr
				}
				return StrictMCPItem{}, &strictRPCError{code: -32600, msg: err.Error()}
			}
			item.HasID = true
			item.ID = idRaw

		case "params":
			vTok, err := dec.Token()
			if err != nil {
				return StrictMCPItem{}, &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid params value: %v", err)}
			}
			if d, ok := vTok.(json.Delim); ok && d == '{' {
				paramsIsObject = true
				pName, pHasName, pNameErr, err := parseStrictParamsObject(dec)
				if err != nil {
					return StrictMCPItem{}, err
				}
				paramsName = pName
				paramsHasName = pHasName
				paramsNameError = pNameErr
			} else {
				if err := skipStrictAfterToken(dec, vTok, 1); err != nil {
					return StrictMCPItem{}, err
				}
			}

		case "result":
			item.HasResult = true
			if err := validateStrictValue(dec, 1); err != nil {
				return StrictMCPItem{}, err
			}

		case "error":
			item.HasError = true
			if err := validateStrictValue(dec, 1); err != nil {
				return StrictMCPItem{}, err
			}

		default:
			if err := validateStrictValue(dec, 1); err != nil {
				return StrictMCPItem{}, err
			}
		}
	}

	endTok, err := dec.Token()
	if err != nil || endTok != json.Delim('}') {
		return StrictMCPItem{}, &strictRPCError{code: -32700, msg: "unterminated JSON-RPC object"}
	}

	if !hasJSONRPC || item.JSONRPC != "2.0" {
		return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: `jsonrpc must be "2.0"`}
	}
	if item.HasMethod {
		if strings.TrimSpace(item.Method) == "" {
			return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: "method must not be empty"}
		}
	} else {
		// Allow valid JSON-RPC responses (id + result or error, not both); reject bare objects missing method.
		if !item.HasID || (item.HasResult == item.HasError) {
			return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: "missing method in JSON-RPC request"}
		}
	}

	if item.Method == "tools/call" {
		if !paramsIsObject {
			return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: "tools/call requires an object params"}
		}
		if paramsNameError != "" {
			return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: paramsNameError}
		}
		if !paramsHasName || strings.TrimSpace(paramsName) == "" {
			return StrictMCPItem{}, &strictRPCError{id: item.ID, code: -32600, msg: "tools/call requires a non-empty string params.name"}
		}
		item.ToolName = paramsName
	}

	return item, nil
}

func parseStrictParamsObject(dec *json.Decoder) (name string, hasName bool, nameErr string, err error) {
	seen := make(map[string]struct{}, 4)
	for dec.More() {
		keyTok, tokErr := dec.Token()
		if tokErr != nil {
			return "", false, "", &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid params key: %v", tokErr)}
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", false, "", &strictRPCError{code: -32700, msg: "params key must be string"}
		}
		if _, dup := seen[key]; dup {
			return "", false, "", &strictRPCError{code: -32600, msg: fmt.Sprintf("duplicate key %q in params", key)}
		}
		seen[key] = struct{}{}

		if strings.EqualFold(key, "name") && key != "name" {
			return "", false, "", &strictRPCError{code: -32600, msg: fmt.Sprintf("non-canonical key %q in params", key)}
		}

		if key == "name" {
			vTok, vErr := dec.Token()
			if vErr != nil {
				return "", false, "", &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid params.name value: %v", vErr)}
			}
			hasName = true
			if s, ok := vTok.(string); ok {
				name = s
			} else {
				nameErr = "params.name must be a string"
				if skipErr := skipStrictAfterToken(dec, vTok, 2); skipErr != nil {
					return "", false, "", skipErr
				}
			}
			continue
		}

		if vErr := validateStrictValue(dec, 2); vErr != nil {
			return "", false, "", vErr
		}
	}
	endTok, endErr := dec.Token()
	if endErr != nil || endTok != json.Delim('}') {
		return "", false, "", &strictRPCError{code: -32700, msg: "unterminated params object"}
	}
	return name, hasName, nameErr, nil
}

func encodeStrictIDToken(tok json.Token) (json.RawMessage, error) {
	switch v := tok.(type) {
	case string:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode id string: %w", err)
		}
		return json.RawMessage(b), nil
	case json.Number:
		return json.RawMessage(v.String()), nil
	case nil:
		return json.RawMessage("null"), nil
	default:
		return nil, errors.New("JSON-RPC id must be string, number, or null")
	}
}

func validateStrictValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid JSON value: %v", err)}
	}
	return skipStrictAfterToken(dec, tok, depth)
}

func skipStrictAfterToken(dec *json.Decoder, tok json.Token, depth int) error {
	if depth > maxStrictJSONDepth {
		return &strictRPCError{code: -32600, msg: "JSON nesting depth exceeded"}
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return &strictRPCError{code: -32700, msg: fmt.Sprintf("invalid nested object key: %v", err)}
			}
			key, ok := kTok.(string)
			if !ok {
				return &strictRPCError{code: -32700, msg: "nested object key must be string"}
			}
			if _, dup := seen[key]; dup {
				return &strictRPCError{code: -32600, msg: fmt.Sprintf("duplicate JSON key %q", key)}
			}
			seen[key] = struct{}{}
			if err := validateStrictValue(dec, depth+1); err != nil {
				return err
			}
		}
		endTok, err := dec.Token()
		if err != nil || endTok != json.Delim('}') {
			return &strictRPCError{code: -32700, msg: "unterminated nested JSON object"}
		}
		return nil

	case '[':
		for dec.More() {
			if err := validateStrictValue(dec, depth+1); err != nil {
				return err
			}
		}
		endTok, err := dec.Token()
		if err != nil || endTok != json.Delim(']') {
			return &strictRPCError{code: -32700, msg: "unterminated nested JSON array"}
		}
		return nil

	default:
		return &strictRPCError{code: -32700, msg: fmt.Sprintf("unexpected JSON delimiter %q", d)}
	}
}

type peekRPCItem struct {
	jsonrpc  string
	method   string
	id       json.RawMessage
	toolName string
	complete bool
}

func parseMCPPeekBytes(raw []byte) MCPPeekInfo {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, utf8BOM))
	if len(trimmed) == 0 {
		return MCPPeekInfo{}
	}

	switch trimmed[0] {
	case '{':
		item, ok := parseSinglePeekObject(trimmed)
		if !ok {
			return MCPPeekInfo{}
		}
		info := MCPPeekInfo{
			IsJSONRPC: true,
			Method:    item.method,
			ID:        cloneRawMessage(item.id),
			ToolName:  item.toolName,
		}
		if item.method == "tools/list" {
			info.HasToolsList = true
		}
		if item.method == "tools/call" {
			info.HasToolsCall = true
			info.ToolCalls = []MCPToolCallRef{{
				ID:   cloneRawMessage(item.id),
				Name: item.toolName,
			}}
		}
		return info

	case '[':
		items := parseBatchPeekObjects(trimmed)
		if len(items) == 0 {
			return MCPPeekInfo{}
		}
		info := MCPPeekInfo{
			IsBatch: true,
		}
		for _, item := range items {
			info.IsJSONRPC = true
			if info.Method == "" && item.method != "" {
				info.Method = item.method
			}
			if len(info.ID) == 0 && len(item.id) > 0 {
				info.ID = cloneRawMessage(item.id)
			}
			if item.method == "tools/list" {
				info.HasToolsList = true
			}
			if item.method == "tools/call" {
				info.HasToolsCall = true
				if info.ToolName == "" && item.toolName != "" {
					info.ToolName = item.toolName
				}
				info.ToolCalls = append(info.ToolCalls, MCPToolCallRef{
					ID:   cloneRawMessage(item.id),
					Name: item.toolName,
				})
			}
		}
		return info

	default:
		return MCPPeekInfo{}
	}
}

func parseSinglePeekObject(data []byte) (peekRPCItem, bool) {
	var parsed struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil {
		if parsed.JSONRPC != "2.0" {
			return peekRPCItem{}, false
		}
		item := peekRPCItem{
			jsonrpc:  parsed.JSONRPC,
			method:   parsed.Method,
			id:       bytes.TrimSpace(parsed.ID),
			complete: true,
		}
		if parsed.Method == "tools/call" && len(parsed.Params) > 0 {
			var p struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(parsed.Params, &p); err == nil {
				item.toolName = p.Name
			}
		}
		return item, true
	}

	// Truncated or partial JSON: scan top-level keys without failing the request.
	item, ok := scanPartialJSONRPCObject(data)
	if !ok {
		return peekRPCItem{}, false
	}
	if item.jsonrpc == "2.0" || (item.jsonrpc == "" && item.method != "") {
		return item, true
	}
	return peekRPCItem{}, false
}

func parseBatchPeekObjects(data []byte) []peekRPCItem {
	// Fast path: full valid JSON array.
	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err == nil {
		out := make([]peekRPCItem, 0, len(rawItems))
		for _, raw := range rawItems {
			raw = bytes.TrimSpace(raw)
			if len(raw) > 0 && raw[0] == '{' {
				if item, ok := parseSinglePeekObject(raw); ok {
					out = append(out, item)
				}
			}
		}
		return out
	}

	// Truncated batch array: walk elements one by one and partially scan any truncated trailing element.
	var out []peekRPCItem
	s := jsonScanner{data: data}
	s.skipWS()
	if !s.consume('[') {
		return nil
	}
	for {
		s.skipWS()
		if s.eof() || s.peek() == ']' {
			break
		}
		start := s.pos
		if s.peek() == '{' {
			if s.skipValue() {
				if item, ok := parseSinglePeekObject(s.data[start:s.pos]); ok {
					out = append(out, item)
				}
			} else {
				// Element is truncated; scan the remaining prefix.
				if item, ok := scanPartialJSONRPCObject(s.data[start:]); ok {
					if item.jsonrpc == "2.0" || (item.jsonrpc == "" && item.method != "") {
						out = append(out, item)
					}
				}
				break
			}
		} else {
			if !s.skipValue() {
				break
			}
		}
		s.skipWS()
		if !s.consume(',') {
			break
		}
	}
	return out
}

func scanPartialJSONRPCObject(data []byte) (peekRPCItem, bool) {
	s := jsonScanner{data: data}
	s.skipWS()
	if !s.consume('{') {
		return peekRPCItem{}, false
	}
	var (
		item     peekRPCItem
		sawField bool
	)
	for {
		s.skipWS()
		if s.eof() || s.peek() == '}' {
			break
		}
		key, ok := s.readString()
		if !ok {
			break
		}
		s.skipWS()
		if !s.consume(':') {
			break
		}
		s.skipWS()
		if s.eof() {
			break
		}
		sawField = true

		switch key {
		case "jsonrpc":
			if val, ok := s.readString(); ok {
				item.jsonrpc = val
			} else {
				return item, sawField
			}
		case "method":
			if val, ok := s.readString(); ok {
				item.method = val
			} else {
				return item, sawField
			}
		case "id":
			valStart := s.pos
			if s.skipValue() {
				raw := bytes.TrimSpace(s.data[valStart:s.pos])
				if json.Valid(raw) {
					item.id = cloneRawMessage(raw)
				}
			} else {
				return item, sawField
			}
		case "params":
			if s.peek() == '{' {
				name, completed := s.scanParamsName()
				if name != "" {
					item.toolName = name
				}
				if !completed {
					return item, sawField
				}
			} else if !s.skipValue() {
				return item, sawField
			}
		default:
			if !s.skipValue() {
				return item, sawField
			}
		}

		s.skipWS()
		if !s.consume(',') {
			break
		}
	}
	return item, sawField
}

// jsonScanner is a minimal, allocation-light JSON token scanner tolerant of truncated inputs.
type jsonScanner struct {
	data []byte
	pos  int
}

func (s *jsonScanner) eof() bool {
	return s.pos >= len(s.data)
}

func (s *jsonScanner) peek() byte {
	if s.pos >= len(s.data) {
		return 0
	}
	return s.data[s.pos]
}

func (s *jsonScanner) skipWS() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\r', '\n':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScanner) consume(b byte) bool {
	if s.pos < len(s.data) && s.data[s.pos] == b {
		s.pos++
		return true
	}
	return false
}

func (s *jsonScanner) readString() (string, bool) {
	start := s.pos
	if !s.skipString() {
		return "", false
	}
	var out string
	if err := json.Unmarshal(s.data[start:s.pos], &out); err != nil {
		return "", false
	}
	return out, true
}

func (s *jsonScanner) skipString() bool {
	if !s.consume('"') {
		return false
	}
	for s.pos < len(s.data) {
		c := s.data[s.pos]
		s.pos++
		if c == '\\' {
			if s.pos >= len(s.data) {
				return false
			}
			s.pos++
			continue
		}
		if c == '"' {
			return true
		}
	}
	return false
}

func (s *jsonScanner) skipValue() bool {
	s.skipWS()
	if s.eof() {
		return false
	}
	switch s.peek() {
	case '"':
		return s.skipString()
	case '{':
		s.pos++
		s.skipWS()
		if s.consume('}') {
			return true
		}
		for {
			s.skipWS()
			if !s.skipString() {
				return false
			}
			s.skipWS()
			if !s.consume(':') {
				return false
			}
			if !s.skipValue() {
				return false
			}
			s.skipWS()
			if s.consume('}') {
				return true
			}
			if !s.consume(',') {
				return false
			}
		}
	case '[':
		s.pos++
		s.skipWS()
		if s.consume(']') {
			return true
		}
		for {
			if !s.skipValue() {
				return false
			}
			s.skipWS()
			if s.consume(']') {
				return true
			}
			if !s.consume(',') {
				return false
			}
		}
	default:
		// Primitive literal: number, true, false, null.
		start := s.pos
		for s.pos < len(s.data) {
			c := s.data[s.pos]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				break
			}
			s.pos++
		}
		if s.pos == start {
			return false
		}
		// If primitive runs right to the end of a truncated buffer without a delimiter,
		// ensure it is valid JSON (e.g. rejects "tru" or "-").
		return json.Valid(s.data[start:s.pos])
	}
}

// scanParamsName scans a JSON object starting at '{' looking for top-level string key "name".
// Returns the discovered name and whether the closing '}' was reached.
func (s *jsonScanner) scanParamsName() (string, bool) {
	if !s.consume('{') {
		return "", false
	}
	var found string
	s.skipWS()
	if s.consume('}') {
		return "", true
	}
	for {
		s.skipWS()
		if s.eof() {
			return found, false
		}
		key, ok := s.readString()
		if !ok {
			return found, false
		}
		s.skipWS()
		if !s.consume(':') {
			return found, false
		}
		s.skipWS()
		if key == "name" {
			if val, ok := s.readString(); ok {
				found = val
			} else {
				return found, false
			}
		} else {
			if !s.skipValue() {
				return found, false
			}
		}
		s.skipWS()
		if s.consume('}') {
			return found, true
		}
		if !s.consume(',') {
			return found, false
		}
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	return cp
}

// ToolFilter implements dual-layer MCP tool visibility and invocation policy.
type ToolFilter struct {
	AllowTools   []string `json:"allowTools,omitempty"`
	DenyTools    []string `json:"denyTools,omitempty"`
	ReadOnlyOnly bool     `json:"readOnlyOnly,omitempty"`
}

// Active reports whether any allow, deny, or read-only restriction is enabled.
func (f ToolFilter) Active() bool {
	return len(f.AllowTools) > 0 || len(f.DenyTools) > 0 || f.ReadOnlyOnly
}

// Allows reports whether toolName with optional annotations.readOnlyHint is permitted.
// Evaluation order:
//  1. If ReadOnlyOnly is true and (readOnlyHint == nil || !*readOnlyHint), deny (fail-closed).
//  2. If AllowTools is non-empty, toolName must match at least one allow pattern.
//  3. If DenyTools matches toolName, deny (deny overrides allow).
func (f ToolFilter) Allows(toolName string, readOnlyHint *bool) bool {
	if f.ReadOnlyOnly && (readOnlyHint == nil || !*readOnlyHint) {
		return false
	}
	if len(f.AllowTools) > 0 {
		allowed := false
		for _, pat := range f.AllowTools {
			if matchToolPattern(pat, toolName) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	for _, pat := range f.DenyTools {
		if matchToolPattern(pat, toolName) {
			return false
		}
	}
	return true
}

func matchToolPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == name {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	// path.Match treats '/' specially (so '*' will not cross '/'). Normalize '/' to NUL
	// when NUL is absent so '*', '?', and '[...]' character classes all work on tool names containing '/'.
	if !strings.ContainsRune(pattern, 0) && !strings.ContainsRune(name, 0) {
		normPat := strings.ReplaceAll(pattern, "/", "\x00")
		normName := strings.ReplaceAll(name, "/", "\x00")
		matched, err := path.Match(normPat, normName)
		if err == nil {
			return matched
		}
	} else if !strings.Contains(name, "/") && !strings.Contains(pattern, "/") {
		matched, err := path.Match(pattern, name)
		if err == nil {
			return matched
		}
	}
	return simpleWildcardMatch(pattern, name)
}

// simpleWildcardMatch performs case-sensitive '*' (0+ runes) and '?' (1 rune) matching.
func simpleWildcardMatch(pattern, text string) bool {
	p := []rune(pattern)
	t := []rune(text)
	pi, ti := 0, 0
	starIdx := -1
	matchIdx := 0

	for ti < len(t) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) {
			pi++
			ti++
		} else if pi < len(p) && p[pi] == '*' {
			starIdx = pi
			matchIdx = ti
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			ti = matchIdx
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

type jsonRPCErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCErrorEnvelope struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Error   jsonRPCErrorBody `json:"error"`
}

func normalizeJSONRPCID(id json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return json.RawMessage("null")
	}
	return trimmed
}

func buildToolDeniedPayload(id json.RawMessage, toolName string) jsonRPCErrorEnvelope {
	return jsonRPCErrorEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeJSONRPCID(id),
		Error: jsonRPCErrorBody{
			Code:    -32601,
			Message: fmt.Sprintf("tool \"%s\" is not permitted for this token/profile", toolName),
		},
	}
}

// WriteToolDeniedJSONRPC writes an HTTP 200 OK JSON-RPC 2.0 -32601 error response
// indicating the requested tool is not permitted for this token/profile.
func WriteToolDeniedJSONRPC(w http.ResponseWriter, id json.RawMessage, toolName string) {
	payload := buildToolDeniedPayload(id, toolName)
	raw, err := marshalCompactJSON(payload)
	if err != nil {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func marshalCompactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// FilterToolsListJSON filters result.tools[] entries in a single JSON-RPC response object
// or JSON-RPC batch array according to f, preserving all other envelope and tool fields intact.
// Returns (out, modified, err). When no tool is removed, modified is false and body is returned as-is.
func FilterToolsListJSON(body []byte, f ToolFilter) ([]byte, bool, error) {
	if !f.Active() {
		return body, false, nil
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(body, utf8BOM))
	if len(trimmed) == 0 {
		return body, false, errors.New("empty tools/list JSON response")
	}

	switch trimmed[0] {
	case '{':
		out, mod, err := filterToolsListObject(trimmed, f)
		if err != nil || !mod {
			return body, false, err
		}
		return out, true, nil

	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return body, false, err
		}
		anyMod := false
		for i, elem := range elems {
			et := bytes.TrimSpace(elem)
			if len(et) == 0 || et[0] != '{' {
				return body, false, errors.New("invalid non-object element in tools/list batch response")
			}
			filtered, mod, err := filterToolsListObject(et, f)
			if err != nil {
				return body, false, err
			}
			if mod {
				elems[i] = filtered
				anyMod = true
			}
		}
		if !anyMod {
			return body, false, nil
		}
		out, err := marshalCompactJSON(elems)
		if err != nil {
			return body, false, err
		}
		return out, true, nil

	default:
		return body, false, errors.New("tools/list response is not a JSON object or array")
	}
}

func filterToolsListObject(raw []byte, f ToolFilter) ([]byte, bool, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, false, err
	}
	resRaw, ok := env["result"]
	if !ok {
		return raw, false, nil
	}
	resTrim := bytes.TrimSpace(resRaw)
	if len(resTrim) == 0 || resTrim[0] != '{' {
		return raw, false, nil
	}

	var resMap map[string]json.RawMessage
	if err := json.Unmarshal(resTrim, &resMap); err != nil {
		return raw, false, err
	}
	toolsRaw, ok := resMap["tools"]
	if !ok {
		return raw, false, nil
	}
	toolsTrim := bytes.TrimSpace(toolsRaw)
	if len(toolsTrim) == 0 || toolsTrim[0] != '[' {
		return raw, false, errors.New("tools/list result.tools is not a JSON array")
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(toolsTrim, &tools); err != nil {
		return raw, false, err
	}

	kept := make([]json.RawMessage, 0, len(tools))
	changed := false
	failClosed := f.ReadOnlyOnly || len(f.AllowTools) > 0
	for _, tRaw := range tools {
		trimmedTool := bytes.TrimSpace(tRaw)
		if len(trimmedTool) == 0 || trimmedTool[0] != '{' {
			if failClosed {
				changed = true
				continue
			}
			kept = append(kept, tRaw)
			continue
		}
		var meta struct {
			Name        string `json:"name"`
			Annotations *struct {
				ReadOnlyHint *bool `json:"readOnlyHint"`
			} `json:"annotations"`
		}
		if err := json.Unmarshal(trimmedTool, &meta); err != nil {
			// Fail closed whenever AllowTools or ReadOnlyOnly is active.
			if failClosed {
				changed = true
				continue
			}
			kept = append(kept, tRaw)
			continue
		}
		if strings.TrimSpace(meta.Name) == "" && failClosed {
			changed = true
			continue
		}
		var roHint *bool
		if meta.Annotations != nil {
			roHint = meta.Annotations.ReadOnlyHint
		}
		if f.Allows(meta.Name, roHint) {
			kept = append(kept, tRaw)
		} else {
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}

	newToolsRaw, err := marshalCompactJSON(kept)
	if err != nil {
		return raw, false, err
	}
	resMap["tools"] = newToolsRaw

	newResRaw, err := marshalCompactJSON(resMap)
	if err != nil {
		return raw, false, err
	}
	env["result"] = newResRaw

	out, err := marshalCompactJSON(env)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

type sseLine struct {
	raw     []byte // line content without trailing EOL
	eol     []byte // "\r\n", "\n", or nil
	isData  bool
	dataVal string
}

// FilterToolsListSSE filters any SSE event whose joined data: payload is JSON containing
// result.tools, preserving event:, id:, retry:, :comment lines and frame delimiters verbatim.
// Supports both \r\n\r\n and \n\n delimiters as well as multi-line data: fields.
func FilterToolsListSSE(body []byte, f ToolFilter) ([]byte, bool, error) {
	if !f.Active() || len(body) == 0 {
		return body, false, nil
	}

	lines := splitSSELines(body)
	var out bytes.Buffer
	out.Grow(len(body))
	anyMod := false

	var firstErr error
	frameStart := 0
	flushFrame := func(frame []sseLine, blank *sseLine) {
		if firstErr != nil {
			return
		}
		if len(frame) == 0 {
			if blank != nil {
				out.Write(blank.raw)
				out.Write(blank.eol)
			}
			return
		}

		var dataParts []string
		firstDataIdx := -1
		for i, ln := range frame {
			if ln.isData {
				if firstDataIdx == -1 {
					firstDataIdx = i
				}
				dataParts = append(dataParts, ln.dataVal)
			}
		}

		if firstDataIdx != -1 {
			joined := strings.Join(dataParts, "\n")
			joinedTrim := bytes.TrimSpace(bytes.TrimPrefix([]byte(joined), utf8BOM))
			if len(joinedTrim) > 0 && (joinedTrim[0] == '{' || joinedTrim[0] == '[') {
				filtered, mod, err := FilterToolsListJSON(joinedTrim, f)
				if err != nil {
					firstErr = err
					return
				}
				if mod {
					anyMod = true
					dataEOL := frame[firstDataIdx].eol
					if len(dataEOL) == 0 {
						dataEOL = []byte("\n")
					}
					for i, ln := range frame {
						if ln.isData {
							if i == firstDataIdx {
								out.WriteString("data: ")
								out.Write(filtered)
								out.Write(dataEOL)
							}
							continue
						}
						out.Write(ln.raw)
						out.Write(ln.eol)
					}
					if blank != nil {
						out.Write(blank.raw)
						out.Write(blank.eol)
					}
					return
				}
			}
		}

		// Untouched frame: emit verbatim.
		for _, ln := range frame {
			out.Write(ln.raw)
			out.Write(ln.eol)
		}
		if blank != nil {
			out.Write(blank.raw)
			out.Write(blank.eol)
		}
	}

	for i := 0; i < len(lines); i++ {
		if len(lines[i].raw) == 0 {
			flushFrame(lines[frameStart:i], &lines[i])
			frameStart = i + 1
		}
	}
	if frameStart < len(lines) {
		flushFrame(lines[frameStart:], nil)
	}
	if firstErr != nil {
		return body, false, firstErr
	}

	if !anyMod {
		return body, false, nil
	}
	return out.Bytes(), true, nil
}

func splitSSELines(body []byte) []sseLine {
	var lines []sseLine
	pos := 0
	for pos < len(body) {
		start := pos
		for pos < len(body) && body[pos] != '\n' && body[pos] != '\r' {
			pos++
		}
		raw := body[start:pos]
		if start == 0 {
			raw = bytes.TrimPrefix(raw, utf8BOM)
		}
		var eol []byte
		if pos < len(body) {
			if body[pos] == '\r' && pos+1 < len(body) && body[pos+1] == '\n' {
				eol = body[pos : pos+2]
				pos += 2
			} else {
				eol = body[pos : pos+1]
				pos++
			}
		}
		ln := sseLine{raw: raw, eol: eol}
		if bytes.HasPrefix(raw, []byte("data:")) {
			ln.isData = true
			val := raw[len("data:"):]
			if len(val) > 0 && val[0] == ' ' {
				val = val[1:]
			}
			ln.dataVal = string(val)
		}
		lines = append(lines, ln)
	}
	return lines
}

// ToolMetric captures per-tool invocation counters and latency summary.
type ToolMetric struct {
	Calls        int64   `json:"calls"`
	Errors       int64   `json:"errors"`
	AvgMs        float64 `json:"avgMs"`
	LastCalledAt string  `json:"lastCalledAt,omitempty"`
}

// MCPMetricsSnapshot is a JSON-serializable point-in-time telemetry snapshot.
type MCPMetricsSnapshot struct {
	TotalRequests  int64                 `json:"totalRequests"`
	ErrorRequests  int64                 `json:"errorRequests"`
	ActiveRequests int64                 `json:"activeRequests"`
	ActiveSSE      int64                 `json:"activeSSE"`
	ActiveSessions int                   `json:"activeSessions"`
	LastRequestAt  string                `json:"lastRequestAt,omitempty"`
	LastTool       string                `json:"lastTool,omitempty"`
	Tools          map[string]ToolMetric `json:"tools,omitempty"`
}

type toolMetricAccum struct {
	calls        int64
	errors       int64
	totalDur     time.Duration
	lastCalledAt time.Time
}

type sessionEntry struct {
	epoch    uint64
	lastSeen time.Time
	stale    bool
}

// MCPMetrics records bounded streaming MCP request, tool, SSE, and session metrics.
type MCPMetrics struct {
	mu             sync.Mutex
	totalRequests  int64
	errorRequests  int64
	activeRequests int64
	activeSSE      int64
	lastRequestAt  time.Time
	lastTool       string
	tools          map[string]*toolMetricAccum
	sessions       map[string]sessionEntry
}

// NewMCPMetrics creates a new thread-safe bounded MCPMetrics collector.
func NewMCPMetrics() *MCPMetrics {
	return &MCPMetrics{
		tools:    make(map[string]*toolMetricAccum),
		sessions: make(map[string]sessionEntry),
	}
}

// MCPResponseRecorder wraps an http.ResponseWriter to observe status, SSE streams,
// Mcp-Session-Id lifecycle, and up to 8 KiB of response bytes without buffering or delaying writes.
type MCPResponseRecorder struct {
	w       http.ResponseWriter
	metrics *MCPMetrics
	peek    MCPPeekInfo
	epoch   uint64

	onSession func(id string, closed bool)

	reqMethod    string
	reqSessionID string

	mu          sync.Mutex
	wroteHeader bool
	statusCode  int
	isSSE       bool
	tap         []byte
}

// SetRequestSession associates the incoming HTTP method and Mcp-Session-Id header
// with this recorder so DELETE or upstream 404 responses evict the closed session.
func (rec *MCPResponseRecorder) SetRequestSession(method, sessionID string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqMethod = method
	rec.reqSessionID = strings.TrimSpace(sessionID)
}

// WrapResponseWriter creates a non-buffering MCPResponseRecorder and a finish callback.
// Callers must invoke finish() (e.g., via defer) once request handling completes.
func (m *MCPMetrics) WrapResponseWriter(w http.ResponseWriter, peek MCPPeekInfo, epoch uint64, onSession func(id string, closed bool)) (*MCPResponseRecorder, func()) {
	start := time.Now()

	m.mu.Lock()
	m.totalRequests++
	m.activeRequests++
	m.lastRequestAt = start.UTC()
	m.mu.Unlock()

	rec := &MCPResponseRecorder{
		w:         w,
		metrics:   m,
		peek:      peek,
		epoch:     epoch,
		onSession: onSession,
	}

	var once sync.Once
	finish := func() {
		once.Do(func() {
			dur := time.Since(start)
			rec.mu.Lock()
			status := rec.statusCode
			isSSE := rec.isSSE
			tapCopy := append([]byte(nil), rec.tap...)
			rec.mu.Unlock()

			isErr := status >= 400 || detectResponseError(tapCopy, isSSE)
			now := time.Now().UTC()

			m.mu.Lock()
			m.activeRequests--
			if m.activeRequests < 0 {
				m.activeRequests = 0
			}
			if isSSE {
				m.activeSSE--
				if m.activeSSE < 0 {
					m.activeSSE = 0
				}
			}
			if isErr {
				m.errorRequests++
			}
			if peek.HasToolsCall && len(peek.ToolCalls) > 0 {
				for _, tc := range peek.ToolCalls {
					if tc.Name != "" {
						m.recordToolLocked(tc.Name, dur, isErr, now)
					}
				}
			} else if peek.ToolName != "" {
				m.recordToolLocked(peek.ToolName, dur, isErr, now)
			}
			m.mu.Unlock()
		})
	}
	return rec, finish
}

// WrapHTTP is a convenience wrapper around WrapResponseWriter that also binds the
// request's HTTP method and Mcp-Session-Id header for automatic DELETE/404 session cleanup.
func (m *MCPMetrics) WrapHTTP(w http.ResponseWriter, r *http.Request, peek MCPPeekInfo, epoch uint64, onSession func(id string, closed bool)) (*MCPResponseRecorder, func()) {
	rec, finish := m.WrapResponseWriter(w, peek, epoch, onSession)
	if r != nil {
		rec.SetRequestSession(r.Method, r.Header.Get("Mcp-Session-Id"))
	}
	return rec, finish
}

func (m *MCPMetrics) recordToolLocked(name string, dur time.Duration, isErr bool, now time.Time) {
	acc, exists := m.tools[name]
	if !exists {
		if len(m.tools) >= maxTrackedTools {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range m.tools {
				if oldestKey == "" || v.lastCalledAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastCalledAt
				}
			}
			if oldestKey != "" {
				delete(m.tools, oldestKey)
			}
		}
		acc = &toolMetricAccum{}
		m.tools[name] = acc
	}
	acc.calls++
	if isErr {
		acc.errors++
	}
	acc.totalDur += dur
	acc.lastCalledAt = now
	m.lastTool = name
}

// Header returns the underlying header map.
func (rec *MCPResponseRecorder) Header() http.Header {
	return rec.w.Header()
}

// WriteHeader records status, SSE state, and Mcp-Session-Id headers before delegating.
func (rec *MCPResponseRecorder) WriteHeader(statusCode int) {
	if statusCode >= 100 && statusCode < 200 {
		rec.w.WriteHeader(statusCode)
		return
	}
	rec.mu.Lock()
	if rec.wroteHeader {
		rec.mu.Unlock()
		return
	}
	rec.wroteHeader = true
	rec.statusCode = statusCode

	ct := rec.w.Header().Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	isSSE := strings.EqualFold(mediaType, "text/event-stream")
	rec.isSSE = isSSE

	respSess := strings.TrimSpace(rec.w.Header().Get("Mcp-Session-Id"))
	reqMethod := rec.reqMethod
	reqSess := rec.reqSessionID
	closeSess := ""
	if reqSess != "" && (statusCode == http.StatusNotFound ||
		(reqMethod == http.MethodDelete && statusCode >= 200 && statusCode < 300)) {
		closeSess = reqSess
	} else if respSess != "" && statusCode == http.StatusNotFound {
		closeSess = respSess
	}
	rec.mu.Unlock()

	if isSSE {
		rec.metrics.mu.Lock()
		rec.metrics.activeSSE++
		rec.metrics.mu.Unlock()
	}

	if closeSess != "" {
		rec.metrics.DeleteSession(closeSess)
		if rec.onSession != nil {
			rec.onSession(closeSess, true)
		}
	} else if respSess != "" && statusCode < 400 {
		rec.metrics.TrackSessionEpoch(respSess, rec.epoch)
		if rec.onSession != nil {
			rec.onSession(respSess, false)
		}
	}

	rec.w.WriteHeader(statusCode)
}

// Write writes directly to the underlying ResponseWriter while non-destructively tapping up to 8 KiB.
func (rec *MCPResponseRecorder) Write(p []byte) (int, error) {
	rec.mu.Lock()
	wrote := rec.wroteHeader
	rec.mu.Unlock()
	if !wrote {
		rec.WriteHeader(http.StatusOK)
	}

	n, err := rec.w.Write(p)
	if n > 0 {
		rec.mu.Lock()
		if rem := maxResponseTapBytes - len(rec.tap); rem > 0 {
			take := n
			if take > rem {
				take = rem
			}
			rec.tap = append(rec.tap, p[:take]...)
		}
		rec.mu.Unlock()
	}
	return n, err
}

// Flush forwards flushing to the underlying ResponseWriter when it implements http.Flusher.
func (rec *MCPResponseRecorder) Flush() {
	if fl, ok := rec.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap returns the underlying http.ResponseWriter so http.ResponseController and
// httputil.ReverseProxy retain streaming flush and deadline capabilities.
func (rec *MCPResponseRecorder) Unwrap() http.ResponseWriter {
	return rec.w
}

// StatusCode returns the HTTP status code written to the response (0 if none written).
func (rec *MCPResponseRecorder) StatusCode() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.statusCode
}

// TrackSessionEpoch records or updates a session ID with its originating process epoch,
// evicting the least-recently-seen entry (preferring expired tombstones) once the bounded capacity (256) is reached.
func (m *MCPMetrics) TrackSessionEpoch(sessionID string, epoch uint64) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[sessionID]; !exists && len(m.sessions) >= maxTrackedSessions {
		var oldestID string
		var oldestEntry sessionEntry
		for id, entry := range m.sessions {
			if oldestID == "" ||
				(entry.stale && !oldestEntry.stale) ||
				(entry.stale == oldestEntry.stale && entry.lastSeen.Before(oldestEntry.lastSeen)) {
				oldestID = id
				oldestEntry = entry
			}
		}
		if oldestID != "" {
			delete(m.sessions, oldestID)
		}
	}
	m.sessions[sessionID] = sessionEntry{
		epoch:    epoch,
		lastSeen: now,
		stale:    false,
	}
}

// CheckSessionEpoch reports whether sessionID is known and whether its epoch differs from currentEpoch or was expired.
func (m *MCPMetrics) CheckSessionEpoch(sessionID string, currentEpoch uint64) (known bool, stale bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[sessionID]
	if !ok {
		return false, false
	}
	entry.lastSeen = time.Now()
	m.sessions[sessionID] = entry
	return true, entry.stale || entry.epoch != currentEpoch
}

// DeleteSession removes sessionID from the tracked session table.
func (m *MCPMetrics) DeleteSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

// ExpireSessions marks all currently tracked sessions as stale so ActiveSessions
// immediately drops to 0 while CheckSessionEpoch can still detect and reject stale session IDs with 404.
func (m *MCPMetrics) ExpireSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, entry := range m.sessions {
		entry.stale = true
		m.sessions[id] = entry
	}
}

func (m *MCPMetrics) activeSessionCountLocked() int {
	n := 0
	for _, entry := range m.sessions {
		if !entry.stale {
			n++
		}
	}
	return n
}

// ActiveSessionCount returns the number of currently active (non-stale) tracked sessions.
func (m *MCPMetrics) ActiveSessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeSessionCountLocked()
}

// ClearSessions removes all tracked sessions.
func (m *MCPMetrics) ClearSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.sessions)
}

// Snapshot returns an immutable point-in-time copy of all metrics.
func (m *MCPMetrics) Snapshot() MCPMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := MCPMetricsSnapshot{
		TotalRequests:  m.totalRequests,
		ErrorRequests:  m.errorRequests,
		ActiveRequests: m.activeRequests,
		ActiveSSE:      m.activeSSE,
		ActiveSessions: m.activeSessionCountLocked(),
		LastTool:       m.lastTool,
	}
	if !m.lastRequestAt.IsZero() {
		snap.LastRequestAt = m.lastRequestAt.Format(time.RFC3339)
	}
	if len(m.tools) > 0 {
		snap.Tools = make(map[string]ToolMetric, len(m.tools))
		for name, acc := range m.tools {
			var avgMs float64
			if acc.calls > 0 {
				avgMs = float64(acc.totalDur.Microseconds()) / 1000.0 / float64(acc.calls)
			}
			tm := ToolMetric{
				Calls:  acc.calls,
				Errors: acc.errors,
				AvgMs:  avgMs,
			}
			if !acc.lastCalledAt.IsZero() {
				tm.LastCalledAt = acc.lastCalledAt.Format(time.RFC3339)
			}
			snap.Tools[name] = tm
		}
	}
	return snap
}

func detectResponseError(tap []byte, isSSE bool) bool {
	if len(tap) == 0 {
		return false
	}
	if isSSE {
		lines := splitSSELines(tap)
		var dataParts []string
		checkFrame := func() bool {
			if len(dataParts) == 0 {
				return false
			}
			joined := strings.Join(dataParts, "\n")
			dataParts = dataParts[:0]
			return jsonPayloadHasError([]byte(joined))
		}
		for _, ln := range lines {
			if len(ln.raw) == 0 {
				if checkFrame() {
					return true
				}
				continue
			}
			if ln.isData {
				dataParts = append(dataParts, ln.dataVal)
			}
		}
		return checkFrame()
	}
	return jsonPayloadHasError(tap)
}

func jsonPayloadHasError(raw []byte) bool {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, utf8BOM))
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{':
		return jsonObjectHasError(trimmed)
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err == nil {
			for _, elem := range elems {
				if jsonObjectHasError(bytes.TrimSpace(elem)) {
					return true
				}
			}
			return false
		}
		// Partial batch fallback: scan elements sequentially.
		s := jsonScanner{data: trimmed}
		s.skipWS()
		if !s.consume('[') {
			return false
		}
		for {
			s.skipWS()
			if s.eof() || s.peek() == ']' {
				break
			}
			start := s.pos
			if s.peek() == '{' {
				if s.skipValue() {
					if jsonObjectHasError(s.data[start:s.pos]) {
						return true
					}
				} else {
					return scanPartialResponseError(s.data[start:])
				}
			} else if !s.skipValue() {
				break
			}
			s.skipWS()
			if !s.consume(',') {
				break
			}
		}
		return false
	default:
		return false
	}
}

func jsonObjectHasError(raw []byte) bool {
	var parsed struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		errTrim := bytes.TrimSpace(parsed.Error)
		if len(errTrim) > 0 && !bytes.Equal(errTrim, []byte("null")) {
			return true
		}
		if parsed.Result != nil && parsed.Result.IsError {
			return true
		}
		return false
	}
	return scanPartialResponseError(raw)
}

func scanPartialResponseError(data []byte) bool {
	s := jsonScanner{data: data}
	s.skipWS()
	if !s.consume('{') {
		return false
	}
	for {
		s.skipWS()
		if s.eof() || s.peek() == '}' {
			return false
		}
		key, ok := s.readString()
		if !ok {
			return false
		}
		s.skipWS()
		if !s.consume(':') {
			return false
		}
		s.skipWS()
		if s.eof() {
			return false
		}
		switch key {
		case "error":
			if bytes.HasPrefix(s.data[s.pos:], []byte("null")) {
				if !s.skipValue() {
					return false
				}
			} else {
				return true
			}
		case "result":
			if s.peek() == '{' {
				isErr, completed := s.scanResultIsError()
				if isErr {
					return true
				}
				if !completed {
					return false
				}
			} else if !s.skipValue() {
				return false
			}
		default:
			if !s.skipValue() {
				return false
			}
		}
		s.skipWS()
		if !s.consume(',') {
			return false
		}
	}
}

func (s *jsonScanner) scanResultIsError() (bool, bool) {
	if !s.consume('{') {
		return false, false
	}
	s.skipWS()
	if s.consume('}') {
		return false, true
	}
	for {
		s.skipWS()
		if s.eof() {
			return false, false
		}
		key, ok := s.readString()
		if !ok {
			return false, false
		}
		s.skipWS()
		if !s.consume(':') {
			return false, false
		}
		s.skipWS()
		if key == "isError" {
			if bytes.HasPrefix(s.data[s.pos:], []byte("true")) {
				return true, true
			}
			if !s.skipValue() {
				return false, false
			}
		} else {
			if !s.skipValue() {
				return false, false
			}
		}
		s.skipWS()
		if s.consume('}') {
			return false, true
		}
		if !s.consume(',') {
			return false, false
		}
	}
}
