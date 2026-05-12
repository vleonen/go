// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"debug/elf"
	"internal/testenv"
	"runtime"
	"testing"
)

func TestEmitRelocsARM64SectionStructure(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	exe, _ := buildEmitRelocsBinary(t, `package main
func main() {}
`)
	ef, err := elf.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer ef.Close()

	var symtabIdx uint32
	for i, sec := range ef.Sections {
		if sec.Name == ".symtab" {
			symtabIdx = uint32(i)
			break
		}
	}
	if symtabIdx == 0 {
		t.Fatal("no .symtab section found")
	}

	for _, sec := range ef.Sections {
		if sec.Type != elf.SHT_RELA {
			continue
		}
		if sec.Entsize != 24 {
			t.Errorf("%s: entsize=%d, want 24", sec.Name, sec.Entsize)
		}
		if sec.Link != symtabIdx {
			t.Errorf("%s: sh_link=%d, want %d (.symtab)", sec.Name, sec.Link, symtabIdx)
		}
		if sec.Info == 0 {
			t.Errorf("%s: sh_info=0, want non-zero target section index", sec.Name)
		}
		if sec.Size > 0 && sec.Size%24 != 0 {
			t.Errorf("%s: size %d is not a multiple of entsize 24", sec.Name, sec.Size)
		}
		if sec.Size > 0 && sec.Info < uint32(len(ef.Sections)) {
			target := ef.Sections[sec.Info]
			if target.Type == elf.SHT_NOBITS {
				t.Errorf("%s: targets NOBITS section %s (should not have relocations)", sec.Name, target.Name)
			}
		}
	}

	for i, sec := range ef.Sections {
		if sec.Type == elf.SHT_NOBITS {
			for _, rs := range ef.Sections {
				if rs.Type == elf.SHT_RELA && rs.Info == uint32(i) {
					t.Errorf("NOBITS section %s has relocation section %s (should not)", sec.Name, rs.Name)
				}
			}
		}
	}
}

func TestEmitRelocsARM64Types(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	_, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

var globalVar = 42

func main() {
	fmt.Println(globalVar)
}
`)

	textRelocs := relocs[".rela.text"]
	if len(textRelocs) == 0 {
		t.Fatal("expected .rela.text to have relocation entries")
	}

	typeCounts := make(map[elf.R_AARCH64]int)
	for _, r := range textRelocs {
		typeCounts[elf.R_AARCH64(r.Type)]++
	}

	expectedTypes := []struct {
		typ  elf.R_AARCH64
		name string
		min  int
	}{
		{elf.R_AARCH64_CALL26, "R_AARCH64_CALL26", 1},
		{elf.R_AARCH64_ADR_PREL_PG_HI21, "R_AARCH64_ADR_PREL_PG_HI21", 1},
		{elf.R_AARCH64_ADD_ABS_LO12_NC, "R_AARCH64_ADD_ABS_LO12_NC", 1},
	}
	for _, exp := range expectedTypes {
		cnt := typeCounts[exp.typ]
		if cnt < exp.min {
			t.Errorf("expected at least %d %s relocations in .rela.text, got %d", exp.min, exp.name, cnt)
		}
	}

	dataRelocs := relocs[".rela.noptrdata"]
	if len(dataRelocs) == 0 {
		dataRelocs = relocs[".rela.data"]
	}
	if len(dataRelocs) == 0 {
		t.Fatal("expected .rela.noptrdata or .rela.data to have relocation entries")
	}

	foundABS64 := false
	for _, r := range dataRelocs {
		if elf.R_AARCH64(r.Type) == elf.R_AARCH64_ABS64 {
			foundABS64 = true
			break
		}
	}
	if !foundABS64 {
		t.Error("expected R_AARCH64_ABS64 in data relocation sections")
	}
}

func TestEmitRelocsARM64ADRPADDPairs(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	_, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`)

	textRelocs := relocs[".rela.text"]

	hi21 := elf.R_AARCH64_ADR_PREL_PG_HI21
	lo12 := elf.R_AARCH64_ADD_ABS_LO12_NC
	ldst8 := elf.R_AARCH64_LDST8_ABS_LO12_NC
	ldst16 := elf.R_AARCH64_LDST16_ABS_LO12_NC
	ldst32 := elf.R_AARCH64_LDST32_ABS_LO12_NC
	ldst64 := elf.R_AARCH64_LDST64_ABS_LO12_NC

	secondTypes := map[uint32]bool{
		uint32(lo12):   true,
		uint32(ldst8):  true,
		uint32(ldst16): true,
		uint32(ldst32): true,
		uint32(ldst64): true,
	}

	pairedCount := 0
	for i := 0; i < len(textRelocs)-1; i++ {
		first := textRelocs[i]
		second := textRelocs[i+1]

		if elf.R_AARCH64(first.Type) != hi21 {
			continue
		}
		if !secondTypes[second.Type] {
			continue
		}
		if first.SymName != second.SymName {
			continue
		}

		if second.Offset != first.Offset+4 {
			t.Errorf("ADRP+ADD pair offsets not sequential: first=%d second=%d (expected %d)",
				first.Offset, second.Offset, first.Offset+4)
			continue
		}

		pairedCount++
	}

	if pairedCount == 0 {
		t.Error("expected at least one ADRP+ADD/LDST pair in .rela.text")
	}
}

func TestEmitRelocsARM64LDSTVariants(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	_, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

var globalVar = 42

func main() {
	fmt.Println(globalVar)
}
`)

	textRelocs := relocs[".rela.text"]
	if len(textRelocs) == 0 {
		t.Fatal("expected .rela.text to have relocation entries")
	}

	ldstTypes := map[uint32]string{
		uint32(elf.R_AARCH64_LDST8_ABS_LO12_NC):  "LDST8",
		uint32(elf.R_AARCH64_LDST16_ABS_LO12_NC): "LDST16",
		uint32(elf.R_AARCH64_LDST32_ABS_LO12_NC): "LDST32",
		uint32(elf.R_AARCH64_LDST64_ABS_LO12_NC): "LDST64",
	}

	ldstCounts := make(map[string]int)
	for _, r := range textRelocs {
		if name, ok := ldstTypes[r.Type]; ok {
			ldstCounts[name]++
		}
	}

	hi21 := uint32(elf.R_AARCH64_ADR_PREL_PG_HI21)
	lo12 := uint32(elf.R_AARCH64_ADD_ABS_LO12_NC)

	addPairCount := 0
	ldstPairCounts := make(map[string]int)
	for i := 0; i < len(textRelocs)-1; i++ {
		first := textRelocs[i]
		second := textRelocs[i+1]
		if first.Type != hi21 {
			continue
		}
		if first.SymName != second.SymName {
			continue
		}
		if second.Offset != first.Offset+4 {
			continue
		}

		if second.Type == lo12 {
			addPairCount++
		} else if name, ok := ldstTypes[second.Type]; ok {
			ldstPairCounts[name]++
		}
	}

	if addPairCount == 0 {
		t.Error("expected at least one ADRP+ADD pair in .rela.text")
	}

	foundLDST := false
	for _, name := range []string{"LDST8", "LDST32", "LDST64"} {
		cnt := ldstCounts[name]
		if cnt > 0 {
			foundLDST = true
			if ldstPairCounts[name] == 0 {
				t.Errorf("found %d %s entries but none in ADRP+LDST pairs", cnt, name)
			}
		}
	}
	if !foundLDST {
		t.Error("expected at least one LDST8, LDST32, or LDST64 relocation in .rela.text")
	}

	t.Logf("LDST counts: %v", ldstCounts)
	t.Logf("ADRP+ADD pairs: %d, ADRP+LDST pairs: %v", addPairCount, ldstPairCounts)
}

func TestEmitRelocsARM64CrossSection(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	exe, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

var globalVar = 42

func main() {
	fmt.Println(globalVar)
}
`)

	ef, err := elf.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer ef.Close()

	sectionByName := make(map[string]*elf.Section)
	for _, sec := range ef.Sections {
		sectionByName[sec.Name] = sec
	}

	symbols, err := ef.Symbols()
	if err != nil {
		t.Fatal(err)
	}

	symSections := make(map[string]string)
	for _, s := range symbols {
		if s.Name == "" || s.Section == elf.SHN_UNDEF || int(s.Section) >= len(ef.Sections) {
			continue
		}
		symSections[s.Name] = ef.Sections[s.Section].Name
	}

	textRelocs := relocs[".rela.text"]
	call26 := uint32(elf.R_AARCH64_CALL26)
	hi21 := uint32(elf.R_AARCH64_ADR_PREL_PG_HI21)

	foundTextToText := false
	foundTextToOther := false
	for _, r := range textRelocs {
		targetSec := symSections[r.SymName]
		if targetSec == "" {
			continue
		}
		if r.Type == call26 {
			if targetSec == ".text" {
				foundTextToText = true
			}
		}
		if r.Type == hi21 {
			if targetSec != ".text" {
				foundTextToOther = true
			}
		}
	}

	if !foundTextToText {
		t.Error("expected CALL26 relocations targeting .text symbols (same-section, enabled by emit-relocs)")
	}
	if !foundTextToOther {
		t.Error("expected ADRP relocations targeting non-.text symbols (cross-section: rodata/data/types)")
	}

	dataRelocs := relocs[".rela.noptrdata"]
	if len(dataRelocs) == 0 {
		dataRelocs = relocs[".rela.data"]
	}
	abs64 := uint32(elf.R_AARCH64_ABS64)

	foundDataToText := false
	for _, r := range dataRelocs {
		if r.Type != abs64 {
			continue
		}
		targetSec := symSections[r.SymName]
		if targetSec == ".text" {
			foundDataToText = true
			break
		}
	}
	if !foundDataToText {
		t.Error("expected ABS64 relocations in data targeting .text symbols (function pointers)")
	}
}

func TestEmitRelocsARM64PerSectionTypes(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	_, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

var globalVar = 42

func main() {
	fmt.Println(globalVar)
}
`)

	textAllowed := map[uint32]bool{
		uint32(elf.R_AARCH64_CALL26):             true,
		uint32(elf.R_AARCH64_ADR_PREL_PG_HI21):   true,
		uint32(elf.R_AARCH64_ADD_ABS_LO12_NC):    true,
		uint32(elf.R_AARCH64_LDST8_ABS_LO12_NC):  true,
		uint32(elf.R_AARCH64_LDST16_ABS_LO12_NC): true,
		uint32(elf.R_AARCH64_LDST32_ABS_LO12_NC): true,
		uint32(elf.R_AARCH64_LDST64_ABS_LO12_NC): true,
	}
	for _, r := range relocs[".rela.text"] {
		if !textAllowed[r.Type] {
			t.Errorf(".rela.text contains unexpected type %d (not CALL26/ADRP/ADD/LDST)", r.Type)
		}
	}

	abs64 := uint32(elf.R_AARCH64_ABS64)
	for _, secName := range []string{".rela.noptrdata", ".rela.data", ".rela.rodata", ".rela.go.type"} {
		entries := relocs[secName]
		if len(entries) == 0 {
			continue
		}
		for _, r := range entries {
			if r.Type != abs64 {
				t.Errorf("%s contains type %d, expected only R_AARCH64_ABS64", secName, r.Type)
			}
		}
	}
}

func TestEmitRelocsARM64Symbols(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	exe, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`)

	ef, err := elf.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer ef.Close()

	symbols, err := ef.Symbols()
	if err != nil {
		t.Fatal(err)
	}

	symtabSec := ef.Section(".symtab")
	if symtabSec == nil {
		t.Fatal("no .symtab section found")
	}
	numSyms := uint32(symtabSec.Size / symtabSec.Entsize)

	for secName, entries := range relocs {
		if len(entries) == 0 {
			continue
		}
		for i, r := range entries {
			if r.SymIdx == 0 {
				t.Errorf("%s entry %d: SymIdx=0 (undefined symbol reference)", secName, i)
			}
			if r.SymIdx >= numSyms {
				t.Errorf("%s entry %d: SymIdx=%d exceeds symbol table size %d", secName, i, r.SymIdx, numSyms)
			}
			if r.SymName == "" && r.SymIdx > 0 && r.SymIdx < uint32(len(symbols)) {
				t.Errorf("%s entry %d: SymName is empty for SymIdx=%d", secName, i, r.SymIdx)
			}
		}

		if entries[0].Offset == 0 && len(entries) > 1 {
			t.Logf("%s: first offset is 0 (expected for section start)", secName)
		}
	}
}

func TestEmitRelocsARM64AddendChecks(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}

	_, relocs := buildEmitRelocsBinary(t, `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`)

	textRelocs := relocs[".rela.text"]
	call26 := uint32(elf.R_AARCH64_CALL26)
	hi21 := uint32(elf.R_AARCH64_ADR_PREL_PG_HI21)

	call26NonZero := 0
	for _, r := range textRelocs {
		if r.Type == call26 && r.Addend != 0 {
			call26NonZero++
		}
	}
	if call26NonZero > 0 {
		t.Logf("CALL26 with non-zero addend: %d entries (may indicate sub-symbol offsets)", call26NonZero)
	}

	hi21Count := 0
	for _, r := range textRelocs {
		if r.Type == hi21 {
			hi21Count++
		}
	}
	if hi21Count == 0 {
		t.Error("expected at least one ADR_PREL_PG_HI21 relocation in .rela.text")
	}

	abs64 := uint32(elf.R_AARCH64_ABS64)
	for _, secName := range []string{".rela.noptrdata", ".rela.data"} {
		entries := relocs[secName]
		if len(entries) == 0 {
			continue
		}
		for _, r := range entries {
			if r.Type == abs64 && r.Addend < 0 {
				t.Errorf("%s: ABS64 with negative addend %d for symbol %s", secName, r.Addend, r.SymName)
			}
		}
	}
}

func TestEmitRelocsARM64BuildModes(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		t.Skip("test only runs on linux/arm64")
	}
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)

	src := `package main

import "fmt"

var globalVar = 42

func main() {
	fmt.Println(globalVar)
}
`

	verifyARM64RelocStructure := func(t *testing.T, exe string, relocs map[string][]elfRelocEntry) {
		t.Helper()
		ef, err := elf.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer ef.Close()

		symtabIdx := uint32(0)
		for i, sec := range ef.Sections {
			if sec.Name == ".symtab" {
				symtabIdx = uint32(i)
				break
			}
		}
		if symtabIdx == 0 {
			t.Fatal("no .symtab section found")
		}

		for _, sec := range ef.Sections {
			if sec.Type != elf.SHT_RELA {
				continue
			}
			if sec.Flags&elf.SHF_ALLOC != 0 {
				continue
			}
			if sec.Entsize != 24 {
				t.Errorf("%s: entsize=%d, want 24", sec.Name, sec.Entsize)
			}
			if sec.Link != symtabIdx {
				t.Errorf("%s: sh_link=%d, want %d (.symtab)", sec.Name, sec.Link, symtabIdx)
			}
			if sec.Info == 0 {
				t.Errorf("%s: sh_info=0, want non-zero target section index", sec.Name)
			}
		}

		textRelocs := relocs[".rela.text"]
		if len(textRelocs) == 0 {
			t.Fatal("expected .rela.text to have relocation entries")
		}

		typeCounts := make(map[elf.R_AARCH64]int)
		for _, r := range textRelocs {
			typeCounts[elf.R_AARCH64(r.Type)]++
		}

		expectedTypes := []struct {
			typ  elf.R_AARCH64
			name string
			min  int
		}{
			{elf.R_AARCH64_CALL26, "R_AARCH64_CALL26", 1},
			{elf.R_AARCH64_ADR_PREL_PG_HI21, "R_AARCH64_ADR_PREL_PG_HI21", 1},
			{elf.R_AARCH64_ADD_ABS_LO12_NC, "R_AARCH64_ADD_ABS_LO12_NC", 1},
		}
		for _, exp := range expectedTypes {
			cnt := typeCounts[exp.typ]
			if cnt < exp.min {
				t.Errorf("expected at least %d %s, got %d", exp.min, exp.name, cnt)
			}
		}

		textAllowed := map[uint32]bool{
			uint32(elf.R_AARCH64_CALL26):             true,
			uint32(elf.R_AARCH64_ADR_PREL_PG_HI21):   true,
			uint32(elf.R_AARCH64_ADD_ABS_LO12_NC):    true,
			uint32(elf.R_AARCH64_LDST8_ABS_LO12_NC):  true,
			uint32(elf.R_AARCH64_LDST16_ABS_LO12_NC): true,
			uint32(elf.R_AARCH64_LDST32_ABS_LO12_NC): true,
			uint32(elf.R_AARCH64_LDST64_ABS_LO12_NC): true,
		}
		for _, r := range textRelocs {
			if !textAllowed[r.Type] {
				t.Errorf(".rela.text contains unexpected type %d", r.Type)
			}
		}
	}

	t.Run("exe", func(t *testing.T) {
		t.Parallel()
		exe, relocs := buildEmitRelocsBinaryWithBuildMode(t, src, "exe")
		verifyARM64RelocStructure(t, exe, relocs)
	})

	t.Run("pie", func(t *testing.T) {
		testenv.MustHaveBuildMode(t, "pie")
		t.Parallel()
		exe, relocs := buildEmitRelocsBinaryWithBuildMode(t, src, "pie")
		verifyARM64RelocStructure(t, exe, relocs)

		ef, err := elf.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer ef.Close()
		if ef.Type != elf.ET_DYN {
			t.Errorf("expected ET_DYN for PIE, got %v", ef.Type)
		}
	})
}
