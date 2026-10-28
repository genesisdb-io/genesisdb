package layout

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFormats_FileNamesAndHeaders(t *testing.T) {
	if FormatCurrent.MainEventLogFile() != "events.gdb" || FormatLegacy.MainEventLogFile() != "events.eventstore" {
		t.Fatal("main event log names")
	}
	if FormatCurrent.LogHeader() != "genesisdb" || FormatLegacy.LogHeader() != "eventstore" {
		t.Fatal("log headers")
	}
	if FormatCurrent.ShardFileName(10) != "0a.gdb" || FormatLegacy.ShardFileName(255) != "ff.eventstore" {
		t.Fatal("shard names")
	}
	for shard := 0; shard < 256; shard++ {
		for _, format := range []Format{FormatCurrent, FormatLegacy} {
			if parsed, ok := format.ParseShardFileName(format.ShardFileName(shard)); !ok || parsed != shard {
				t.Fatalf("%s shard %d", format, shard)
			}
		}
	}
	for _, invalid := range []string{"0A.gdb", "100.gdb", "zz.gdb", "00.gdbx", "00.eventstore"} {
		if _, ok := FormatCurrent.ParseShardFileName(invalid); ok {
			t.Errorf("accepted %q", invalid)
		}
	}
	if !IsLogHeader([]byte("genesisdb")) || !IsLogHeader([]byte("eventstore")) || IsLogHeader([]byte("other")) {
		t.Fatal("IsLogHeader")
	}
	if !IsLogHeaderPrefix([]byte("gene")) || !IsLogHeaderPrefix([]byte("eventst")) || IsLogHeaderPrefix([]byte("{")) {
		t.Fatal("IsLogHeaderPrefix")
	}
	if !FormatLegacy.IsRegularEventLogFile("events.eventstore") || FormatLegacy.IsRegularEventLogFile("refs.eventstore") || FormatCurrent.IsRegularEventLogFile("events.eventstore") {
		t.Fatal("IsRegularEventLogFile")
	}
	if format, err := ParseFormat("legacy"); err != nil || format != FormatLegacy {
		t.Fatal("ParseFormat")
	}
	for name, expected := range map[string]Format{"current": FormatCurrent, "GDB": FormatCurrent, " eventstore ": FormatLegacy} {
		if format, err := ParseFormat(name); err != nil || format != expected {
			t.Errorf("ParseFormat(%q) = %v, %v", name, format, err)
		}
	}
	if _, err := ParseFormat("v3"); err == nil {
		t.Error("unknown format accepted")
	}
	if FormatCurrent.String() != "current" || FormatLegacy.String() != "legacy" {
		t.Error("String")
	}
}

func TestFormats_LogFileSets(t *testing.T) {
	for _, format := range []Format{FormatCurrent, FormatLegacy} {
		files := format.LogFiles()
		if len(files) != 3 || files[2] != format.MainEventLogFile() || files[0] != format.ReferenceDataLogFile() || files[1] != format.SystemEventLogFile() {
			t.Errorf("%s LogFiles %v", format, files)
		}
		for _, file := range files {
			if !format.IsLogFile(file) {
				t.Errorf("%s is not a log file of %s", file, format)
			}
		}
		if format.IsLogFile(format.Extension()) {
			t.Error("a bare extension is not a log file")
		}
		if format.LogBaseName(format.MainEventLogFile()) != "events" {
			t.Error("LogBaseName")
		}
		if string(format.LogHeaderLine()) != format.LogHeader()+"\n" {
			t.Error("LogHeaderLine")
		}
		if format.TemporaryIndexStateFile() != format.IndexStateFile()+TemporaryFileSuffix {
			t.Error("TemporaryIndexStateFile")
		}
	}
	if FormatCurrent.IndexCatalogFile() != "index.gdb" || FormatLegacy.IndexStateFile() != "index-state.eventstore" {
		t.Error("index file names")
	}
}

func TestLegacyIndexFileNames(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !LegacyHashedIndexFile(hash + ".eventstore") {
		t.Error("hashed legacy file rejected")
	}
	for _, invalid := range []string{hash + ".gdb", hash[:63] + ".eventstore", "index.eventstore", hash + "X.eventstore"} {
		if LegacyHashedIndexFile(invalid) {
			t.Errorf("accepted %q", invalid)
		}
	}
	if LegacyIndexFile != "index.eventstore" {
		t.Error("LegacyIndexFile")
	}
	if !IsHex64(hash) || IsHex64(hash[:63]) || IsHex64(hash[:63]+"G") || IsHex64(hash[:63]+"A") {
		t.Error("IsHex64")
	}
}

func TestFormatMarker_RoundTrip(t *testing.T) {
	directory := t.TempDir()
	if _, err := ReadFormatMarker(directory); !errors.Is(err, ErrNoFormatMarker) {
		t.Fatalf("missing marker: %v", err)
	}

	marker := NewFormatMarker()
	if marker.Format != "genesisdb" || marker.Version != CurrentFormatVersion || marker.IsConverting() {
		t.Fatalf("new marker %+v", marker)
	}
	if string(marker.Encode()) != `{"format":"genesisdb","version":4}`+"\n" {
		t.Fatalf("marker encoding changed: %s", marker.Encode())
	}
	path := filepath.Join(directory, FormatMarkerFile)
	if err := os.WriteFile(path, marker.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := ReadFormatMarker(directory)
	if err != nil || read != marker {
		t.Fatalf("read %+v, %v", read, err)
	}

	converting := NewFormatMarker()
	converting.Conversion = ConversionInProgress
	converting.Target = FormatLegacy.String()
	if err := os.WriteFile(path, converting.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err = ReadFormatMarker(directory)
	if err != nil || !read.IsConverting() || read.ConversionTarget() != FormatLegacy {
		t.Fatalf("converting marker %+v, %v", read, err)
	}
	if (FormatMarker{Target: "anything else"}).ConversionTarget() != FormatCurrent {
		t.Error("unknown target must default to the current format")
	}

	for name, content := range map[string]string{"not json": "garbage", "other product": `{"format":"sqlite","version":3}`} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFormatMarker(directory); err == nil || errors.Is(err, ErrNoFormatMarker) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
