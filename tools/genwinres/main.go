//go:build ignore

// genwinres generates pure-Go Windows COFF resource object files (rsrc_windows_amd64.syso
// and rsrc_windows_arm64.syso) for cmd/mcp-hub and cmd/mcp-hub-ctl with zero external dependencies.
//
// Embedded PE resources:
//   - RT_ICON (3) & RT_GROUP_ICON (14): Multi-size PNG-encoded Tri-Node Hub app icon (16, 20, 24, 32, 48, 256 px)
//   - RT_VERSION (16): VS_VERSION_INFO reading Version from internal/hub/version.go
//   - RT_MANIFEST (24): Windows 10/11 compatibility, asInvoker, PerMonitorV2 DPI, longPathAware, UTF-8
//
// Usage:
//
//	go run ./tools/genwinres/main.go
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub/trayui"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	rtManifest  = 24

	langEnUS = 0x0409

	machineAMD64 = 0x8664
	machineARM64 = 0xAA64

	relocAMD64Addr32NB = 0x0003
	relocARM64Addr32NB = 0x0002

	scnRsrcChars = 0x40000040 // IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
)

type targetSpec struct {
	dir         string
	binaryName  string
	internal    string
	description string
}

type resEntry struct {
	typeID uint32
	nameID uint32
	langID uint32
	data   []byte
}

func main() {
	rootDir := flag.String("root", ".", "Repository root directory")
	versionFlag := flag.String("version", "", "Override version (defaults to Version in internal/hub/version.go)")
	flag.Parse()

	ver := strings.TrimSpace(*versionFlag)
	if ver == "" {
		var err error
		ver, err = readVersionFromSource(filepath.Join(*rootDir, "internal", "hub", "version.go"))
		if err != nil {
			fatalf("read version.go: %v", err)
		}
	}

	// Render multi-size PNG-encoded Tri-Node Hub app icons once and share across targets.
	iconSizes := []int{16, 20, 24, 32, 48, 256}
	iconPNGs := make([][]byte, len(iconSizes))
	for i, sz := range iconSizes {
		c := trayui.RenderAppIcon(sz)
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, c.ToNRGBA()); err != nil {
			fatalf("encode %dpx PNG icon: %v", sz, err)
		}
		iconPNGs[i] = buf.Bytes()
	}
	groupIcon := buildGroupIconDir(iconSizes, iconPNGs)

	targets := []targetSpec{
		{
			dir:         filepath.Join(*rootDir, "cmd", "mcp-hub"),
			binaryName:  "mcp-hub.exe",
			internal:    "mcp-hub",
			description: "McpHub Local MCP Daemon Supervisor",
		},
		{
			dir:         filepath.Join(*rootDir, "cmd", "mcp-hub-ctl"),
			binaryName:  "mcp-hub-ctl.exe",
			internal:    "mcp-hub-ctl",
			description: "McpHub Control CLI",
		},
	}

	arches := []struct {
		goarch    string
		machine   uint16
		relocType uint16
	}{
		{"amd64", machineAMD64, relocAMD64Addr32NB},
		{"arm64", machineARM64, relocARM64Addr32NB},
	}

	for _, tgt := range targets {
		manifest := buildManifest(tgt.internal, ver)
		versionInfo := buildVersionInfo(ver, tgt)

		var entries []resEntry
		for i, pngBytes := range iconPNGs {
			entries = append(entries, resEntry{
				typeID: rtIcon,
				nameID: uint32(i + 1),
				langID: langEnUS,
				data:   pngBytes,
			})
		}
		entries = append(entries,
			resEntry{typeID: rtGroupIcon, nameID: 1, langID: langEnUS, data: groupIcon},
			resEntry{typeID: rtVersion, nameID: 1, langID: langEnUS, data: versionInfo},
			resEntry{typeID: rtManifest, nameID: 1, langID: langEnUS, data: manifest},
		)

		rsrcBytes, relocOffsets := buildRsrcSection(entries)

		for _, arch := range arches {
			coff := buildCOFFObject(arch.machine, arch.relocType, rsrcBytes, relocOffsets)
			outPath := filepath.Join(tgt.dir, fmt.Sprintf("rsrc_windows_%s.syso", arch.goarch))
			if err := os.WriteFile(outPath, coff, 0o644); err != nil {
				fatalf("write %s: %v", outPath, err)
			}
			fmt.Printf("wrote %s (%d bytes, version=%s)\n", outPath, len(coff), ver)
		}
	}
}

func readVersionFromSource(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`Version\s*=\s*"([^"]+)"`)
	m := re.FindSubmatch(raw)
	if len(m) < 2 {
		return "", fmt.Errorf("Version constant not found in %s", path)
	}
	return strings.TrimSpace(string(m[1])), nil
}

func parseVersionQuads(ver string) [4]uint16 {
	clean := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	if idx := strings.IndexAny(clean, "-+"); idx >= 0 {
		clean = clean[:idx]
	}
	parts := strings.Split(clean, ".")
	var out [4]uint16
	for i := 0; i < len(parts) && i < 4; i++ {
		n, err := strconv.Atoi(parts[i])
		if err == nil && n >= 0 && n <= 65535 {
			out[i] = uint16(n)
		}
	}
	return out
}

func buildGroupIconDir(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	// GRPICONDIR header (6 bytes)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))          // idReserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))          // idType = ICO
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(sizes))) // idCount

	// GRPICONDIRENTRY (14 bytes each)
	for i, sz := range sizes {
		var dim byte
		if sz < 256 {
			dim = byte(sz)
		}
		buf.WriteByte(dim)                                                 // bWidth (0 => 256)
		buf.WriteByte(dim)                                                 // bHeight (0 => 256)
		buf.WriteByte(0)                                                   // bColorCount
		buf.WriteByte(0)                                                   // bReserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))             // wPlanes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32))            // wBitCount
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngs[i])))  // dwBytesInRes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(i+1))           // nID (RT_ICON ID)
	}
	return buf.Bytes()
}

func buildManifest(appName, ver string) []byte {
	q := parseVersionQuads(ver)
	asmVer := fmt.Sprintf("%d.%d.%d.%d", q[0], q[1], q[2], q[3])
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="%s" processorArchitecture="*" name="McpHub.%s" type="win32"/>
  <description>McpHub Local MCP Daemon</description>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <!-- Windows 10 and Windows 11 -->
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
      <!-- Windows 8.1 -->
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <!-- Windows 8 -->
      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>
      <!-- Windows 7 -->
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2, PerMonitor</dpiAwareness>
      <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
      <activeCodePage xmlns="http://schemas.microsoft.com/SMI/2019/WindowsSettings">UTF-8</activeCodePage>
    </windowsSettings>
  </application>
</assembly>
`, asmVer, appName)
	return []byte(xml)
}

func buildVersionInfo(ver string, tgt targetSpec) []byte {
	q := parseVersionQuads(ver)
	verQuadStr := fmt.Sprintf("%d.%d.%d.%d", q[0], q[1], q[2], q[3])

	// VS_FIXEDFILEINFO (52 bytes)
	var ffi bytes.Buffer
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0xFEEF04BD))                      // dwSignature
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0x00010000))                      // dwStrucVersion
	_ = binary.Write(&ffi, binary.LittleEndian, (uint32(q[0])<<16)|uint32(q[1]))         // dwFileVersionMS
	_ = binary.Write(&ffi, binary.LittleEndian, (uint32(q[2])<<16)|uint32(q[3]))         // dwFileVersionLS
	_ = binary.Write(&ffi, binary.LittleEndian, (uint32(q[0])<<16)|uint32(q[1]))         // dwProductVersionMS
	_ = binary.Write(&ffi, binary.LittleEndian, (uint32(q[2])<<16)|uint32(q[3]))         // dwProductVersionLS
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0x0000003F))                      // dwFileFlagsMask
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0))                               // dwFileFlags
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0x00040004))                      // dwFileOS (VOS_NT_WINDOWS32)
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0x00000001))                      // dwFileType (VFT_APP)
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0))                               // dwFileSubtype
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0))                               // dwFileDateMS
	_ = binary.Write(&ffi, binary.LittleEndian, uint32(0))                               // dwFileDateLS

	kvPairs := [][2]string{
		{"CompanyName", "McpHub Contributors"},
		{"FileDescription", tgt.description},
		{"FileVersion", verQuadStr},
		{"InternalName", tgt.internal},
		{"LegalCopyright", "Copyright (c) 2026 mcp-hub contributors (MIT)"},
		{"OriginalFilename", tgt.binaryName},
		{"ProductName", "McpHub"},
		{"ProductVersion", ver},
	}

	var stringChildren [][]byte
	for _, kv := range kvPairs {
		valUnits := utf16Nul(kv[1])
		valBytes := encodeUTF16LE(valUnits)
		// For String structures (wType=1), wValueLength is in UTF-16 code units!
		stringChildren = append(stringChildren, buildVerNode(kv[0], 1, uint16(len(valUnits)), valBytes, nil))
	}
	stringTable := buildVerNode("040904b0", 1, 0, nil, stringChildren)
	stringFileInfo := buildVerNode("StringFileInfo", 1, 0, nil, [][]byte{stringTable})

	var transVal bytes.Buffer
	_ = binary.Write(&transVal, binary.LittleEndian, uint16(langEnUS))
	_ = binary.Write(&transVal, binary.LittleEndian, uint16(0x04B0)) // Unicode codepage 1200
	translation := buildVerNode("Translation", 0, uint16(transVal.Len()), transVal.Bytes(), nil)
	varFileInfo := buildVerNode("VarFileInfo", 1, 0, nil, [][]byte{translation})

	return buildVerNode("VS_VERSION_INFO", 0, uint16(ffi.Len()), ffi.Bytes(), [][]byte{stringFileInfo, varFileInfo})
}

func buildVerNode(key string, wType uint16, wValueLength uint16, value []byte, children [][]byte) []byte {
	var body bytes.Buffer
	// Placeholder for wLength (2), wValueLength (2), wType (2)
	_ = binary.Write(&body, binary.LittleEndian, uint16(0))
	_ = binary.Write(&body, binary.LittleEndian, wValueLength)
	_ = binary.Write(&body, binary.LittleEndian, wType)
	body.Write(encodeUTF16LE(utf16Nul(key)))
	pad4(&body)
	if len(value) > 0 {
		body.Write(value)
	}
	for _, ch := range children {
		pad4(&body)
		body.Write(ch)
	}
	out := body.Bytes()
	binary.LittleEndian.PutUint16(out[0:2], uint16(len(out)))
	return out
}

func utf16Nul(s string) []uint16 {
	u := utf16.Encode([]rune(s))
	return append(u, 0)
}

func encodeUTF16LE(u []uint16) []byte {
	out := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

func pad4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// buildRsrcSection constructs a canonical 3-level PE .rsrc tree (Type -> ID -> Lang -> DataEntry -> Payloads)
// and returns the raw section bytes plus the offsets of every IMAGE_RESOURCE_DATA_ENTRY.OffsetToData field
// that requires an ADDR32NB relocation against the .rsrc section symbol.
func buildRsrcSection(entries []resEntry) ([]byte, []uint32) {
	type langLeaf struct {
		langID uint32
		data   []byte
	}
	type nameNode struct {
		nameID uint32
		langs  []langLeaf
	}
	type typeNode struct {
		typeID uint32
		names  []nameNode
	}

	// Group and sort entries ascending at every level as required by PE/COFF spec.
	typeMap := map[uint32]map[uint32][]langLeaf{}
	for _, e := range entries {
		if typeMap[e.typeID] == nil {
			typeMap[e.typeID] = map[uint32][]langLeaf{}
		}
		typeMap[e.typeID][e.nameID] = append(typeMap[e.typeID][e.nameID], langLeaf{langID: e.langID, data: e.data})
	}

	var types []typeNode
	for tid, nMap := range typeMap {
		var names []nameNode
		for nid, langs := range nMap {
			sort.Slice(langs, func(i, j int) bool { return langs[i].langID < langs[j].langID })
			names = append(names, nameNode{nameID: nid, langs: langs})
		}
		sort.Slice(names, func(i, j int) bool { return names[i].nameID < names[j].nameID })
		types = append(types, typeNode{typeID: tid, names: names})
	}
	sort.Slice(types, func(i, j int) bool { return types[i].typeID < types[j].typeID })

	// Compute deterministic offsets for all directories, data entries, and payloads.
	const (
		dirHeaderSize = 16
		dirEntrySize  = 8
		dataEntrySize = 16
	)

	offset := uint32(0)
	// Level 1: root directory
	offset += dirHeaderSize + uint32(len(types))*dirEntrySize

	// Level 2: one directory per type
	typeDirOffsets := make([]uint32, len(types))
	for ti, tn := range types {
		typeDirOffsets[ti] = offset
		offset += dirHeaderSize + uint32(len(tn.names))*dirEntrySize
	}

	// Level 3: one directory per (type, name)
	nameDirOffsets := make([][]uint32, len(types))
	totalLeaves := 0
	for ti, tn := range types {
		nameDirOffsets[ti] = make([]uint32, len(tn.names))
		for ni, nn := range tn.names {
			nameDirOffsets[ti][ni] = offset
			offset += dirHeaderSize + uint32(len(nn.langs))*dirEntrySize
			totalLeaves += len(nn.langs)
		}
	}

	// Data entries table
	dataEntryOffsets := make([][][]uint32, len(types))
	for ti, tn := range types {
		dataEntryOffsets[ti] = make([][]uint32, len(tn.names))
		for ni, nn := range tn.names {
			dataEntryOffsets[ti][ni] = make([]uint32, len(nn.langs))
			for li := range nn.langs {
				dataEntryOffsets[ti][ni][li] = offset
				offset += dataEntrySize
			}
		}
	}

	// Payloads (4-byte aligned)
	payloadOffsets := make([][][]uint32, len(types))
	for ti, tn := range types {
		payloadOffsets[ti] = make([][]uint32, len(tn.names))
		for ni, nn := range tn.names {
			payloadOffsets[ti][ni] = make([]uint32, len(nn.langs))
			for li, lf := range nn.langs {
				for offset%4 != 0 {
					offset++
				}
				payloadOffsets[ti][ni][li] = offset
				offset += uint32(len(lf.data))
			}
		}
	}

	var buf bytes.Buffer
	writeDirHeader := func(nEntries int) {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0))        // Characteristics
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0))        // TimeDateStamp
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))        // MajorVersion
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))        // MinorVersion
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))        // NumberOfNamedEntries
		_ = binary.Write(&buf, binary.LittleEndian, uint16(nEntries)) // NumberOfIdEntries
	}

	// Write Level 1
	writeDirHeader(len(types))
	for ti, tn := range types {
		_ = binary.Write(&buf, binary.LittleEndian, tn.typeID)
		_ = binary.Write(&buf, binary.LittleEndian, typeDirOffsets[ti]|0x80000000)
	}

	// Write Level 2
	for ti, tn := range types {
		writeDirHeader(len(tn.names))
		for ni, nn := range tn.names {
			_ = binary.Write(&buf, binary.LittleEndian, nn.nameID)
			_ = binary.Write(&buf, binary.LittleEndian, nameDirOffsets[ti][ni]|0x80000000)
		}
	}

	// Write Level 3
	for ti, tn := range types {
		for ni, nn := range tn.names {
			writeDirHeader(len(nn.langs))
			for li, lf := range nn.langs {
				_ = binary.Write(&buf, binary.LittleEndian, lf.langID)
				_ = binary.Write(&buf, binary.LittleEndian, dataEntryOffsets[ti][ni][li])
			}
		}
	}

	// Write Data Entries and record ADDR32NB relocation offsets.
	relocOffsets := make([]uint32, 0, totalLeaves)
	for ti, tn := range types {
		for ni, nn := range tn.names {
			for li, lf := range nn.langs {
				relocOffsets = append(relocOffsets, uint32(buf.Len()))
				_ = binary.Write(&buf, binary.LittleEndian, payloadOffsets[ti][ni][li]) // OffsetToData (relocated)
				_ = binary.Write(&buf, binary.LittleEndian, uint32(len(lf.data)))       // Size
				_ = binary.Write(&buf, binary.LittleEndian, uint32(0))                  // CodePage
				_ = binary.Write(&buf, binary.LittleEndian, uint32(0))                  // Reserved
			}
		}
	}

	// Write Payloads
	for _, tn := range types {
		for _, nn := range tn.names {
			for _, lf := range nn.langs {
				pad4(&buf)
				buf.Write(lf.data)
			}
		}
	}
	pad4(&buf)

	return buf.Bytes(), relocOffsets
}

func buildCOFFObject(machine uint16, relocType uint16, rsrcData []byte, relocOffsets []uint32) []byte {
	const (
		fileHdrSize    = 20
		secHdrSize     = 40
		relocSize      = 10
		rawDataPointer = fileHdrSize + secHdrSize
	)

	relocPointer := uint32(rawDataPointer + len(rsrcData))
	symTablePointer := relocPointer + uint32(len(relocOffsets)*relocSize)

	var buf bytes.Buffer

	// IMAGE_FILE_HEADER (20 bytes)
	_ = binary.Write(&buf, binary.LittleEndian, machine)         // Machine
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))       // NumberOfSections
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))       // TimeDateStamp
	_ = binary.Write(&buf, binary.LittleEndian, symTablePointer) // PointerToSymbolTable
	_ = binary.Write(&buf, binary.LittleEndian, uint32(1))       // NumberOfSymbols
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))       // SizeOfOptionalHeader
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))       // Characteristics

	// IMAGE_SECTION_HEADER (40 bytes)
	secName := [8]byte{'.', 'r', 's', 'r', 'c', 0, 0, 0}
	buf.Write(secName[:])
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))                 // VirtualSize
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))                 // VirtualAddress
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(rsrcData)))     // SizeOfRawData
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rawDataPointer))    // PointerToRawData
	_ = binary.Write(&buf, binary.LittleEndian, relocPointer)              // PointerToRelocations
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))                 // PointerToLinenumbers
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(relocOffsets))) // NumberOfRelocations
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))                 // NumberOfLinenumbers
	_ = binary.Write(&buf, binary.LittleEndian, uint32(scnRsrcChars))      // Characteristics

	// Raw .rsrc section data
	buf.Write(rsrcData)

	// IMAGE_RELOCATION entries (10 bytes each)
	for _, off := range relocOffsets {
		_ = binary.Write(&buf, binary.LittleEndian, off)       // VirtualAddress
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // SymbolTableIndex (.rsrc)
		_ = binary.Write(&buf, binary.LittleEndian, relocType) // Type
	}

	// Symbol table: 1 section symbol for ".rsrc" (18 bytes)
	buf.Write(secName[:])
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // Value
	_ = binary.Write(&buf, binary.LittleEndian, int16(1))  // SectionNumber (1-based)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // Type
	buf.WriteByte(3)                                       // StorageClass = IMAGE_SYM_CLASS_STATIC
	buf.WriteByte(0)                                       // NumberOfAuxSymbols

	// Empty COFF string table (4 bytes: size = 4)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4))

	return buf.Bytes()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genwinres: "+format+"\n", args...)
	os.Exit(1)
}
