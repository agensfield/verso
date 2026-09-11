package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	targets        = []target{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}
	buildBinary    = realBuildBinary
)

type target struct{ os, arch string }

func main() {
	var version, commit, output string
	flag.StringVar(&version, "version", "", "release version, with or without v prefix")
	flag.StringVar(&commit, "commit", "", "source commit SHA")
	flag.StringVar(&output, "output", "dist", "artifact output directory")
	flag.Parse()
	if err := run(version, commit, output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(version, commit, output string) error {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	commit = strings.TrimSpace(commit)
	if !versionPattern.MatchString(version) {
		return errors.New("release version must be semantic")
	}
	if !commitPattern.MatchString(commit) {
		return errors.New("release commit must be a 7-40 character lowercase hex SHA")
	}
	if output == "" {
		return errors.New("release output directory is required")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "verso-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var checksums []string
	for _, target := range targets {
		binary := filepath.Join(tmp, target.os+"-"+target.arch, "verso")
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			return err
		}
		if err := buildBinary(binary, version, commit, target); err != nil {
			return fmt.Errorf("build %s/%s: %w", target.os, target.arch, err)
		}
		name := fmt.Sprintf("verso_%s_%s_%s.tar.gz", version, target.os, target.arch)
		archivePath := filepath.Join(output, name)
		if err := writeArchive(archivePath, binary); err != nil {
			return fmt.Errorf("package %s/%s: %w", target.os, target.arch, err)
		}
		digest, err := fileDigest(archivePath)
		if err != nil {
			return err
		}
		checksums = append(checksums, digest+"  "+name)
	}
	sort.Strings(checksums)
	return os.WriteFile(filepath.Join(output, "checksums.txt"), []byte(strings.Join(checksums, "\n")+"\n"), 0o644)
}

func realBuildBinary(binary, version, commit string, target target) error {
	ldflags := fmt.Sprintf("-s -w -buildid= -X github.com/agensfield/verso/internal/buildinfo.version=v%s -X github.com/agensfield/verso/internal/buildinfo.commit=%s -X github.com/agensfield/verso/internal/buildinfo.installKind=release", version, commit)
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binary, "./cmd/verso")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.os, "GOARCH="+target.arch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func writeArchive(path, binary string) (retErr error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	zipper, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	zipper.ModTime, zipper.OS = time.Unix(0, 0).UTC(), 255
	archive := tar.NewWriter(zipper)
	header := &tar.Header{Name: "verso", Mode: 0o755, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	if _, err := archive.Write(data); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return zipper.Close()
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
