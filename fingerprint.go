package main

// Install fingerprint ("version tag").
//
// Ikemen's netplay only exchanges inputs, so both players must have exactly
// the same engine, characters, stages and scripts or the match desyncs. The
// engine doesn't check this itself, so Ikemen Party does: it hashes the
// install and the lobby only lets matching installs join each other.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Folders that affect gameplay. save/, sound/ and video/ don't.
var fingerprintDirs = []string{"chars", "stages", "data", "font", "external"}

const fingerprintFullHashLimit = 2 << 20 // files above 2 MB: size only

func computeFingerprint(ikemenDir string) (string, error) {
	type item struct {
		rel  string // original path, for opening
		key  string // lower-case, for stable ordering across systems
		size int64
	}
	var files []item
	for _, d := range fingerprintDirs {
		root := filepath.Join(ikemenDir, d)
		_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(ikemenDir, p)
			key := strings.ToLower(filepath.ToSlash(rel))
			// Our own mod and its exchange files must not change the tag.
			if strings.HasPrefix(key, "external/mods/ikemen_party") {
				return nil
			}
			if info, err := de.Info(); err == nil {
				files = append(files, item{rel, key, info.Size()})
			}
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].key < files[j].key })

	h := sha256.New()
	for _, exe := range []string{"Ikemen_GO.exe", "Ikemen_GO", "Ikemen_GO.command"} {
		if f, err := os.Open(filepath.Join(ikemenDir, exe)); err == nil {
			io.WriteString(h, "exe:"+exe+"\n")
			_, _ = io.Copy(h, f)
			f.Close()
			break
		}
	}
	for _, f := range files {
		io.WriteString(h, f.key+"\x00")
		if f.size <= fingerprintFullHashLimit {
			if fh, err := os.Open(filepath.Join(ikemenDir, f.rel)); err == nil {
				_, _ = io.Copy(h, fh)
				fh.Close()
			}
		} else {
			io.WriteString(h, "size:"+strconv.FormatInt(f.size, 10))
		}
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))[:8], nil
}

// findIkemenDir returns the Ikemen GO folder: the flag value, else the
// folder holding this exe or the current folder, if Ikemen_GO is there.
func findIkemenDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	cands := []string{exeDir()}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, wd)
	}
	for _, d := range cands {
		for _, exe := range []string{"Ikemen_GO.exe", "Ikemen_GO", "Ikemen_GO.command"} {
			if _, err := os.Stat(filepath.Join(d, exe)); err == nil {
				return d
			}
		}
	}
	return ""
}
