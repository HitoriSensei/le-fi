package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
)

// streamTarGz walks dir and writes a gzip-compressed tar archive of its
// contents to w. Fully streamed: gzip.Writer and tar.Writer both write
// through to w as they go, so this works for a directory of any size
// without buffering the archive anywhere.
func streamTarGz(w io.Writer, dir string) error {
	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)

	base := filepath.Clean(dir)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			return copyFileInto(tw, path)
		}
		return nil
	})
	if walkErr != nil {
		// Still try to close so a partial-but-valid archive is possible;
		// the walk error is what actually matters to the caller.
		_ = tw.Close()
		_ = gzw.Close()
		return walkErr
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gzw.Close()
}

// copyFileInto is its own function (rather than inline in the Walk
// callback) so the deferred Close runs after each file instead of piling
// up until the whole walk finishes.
func copyFileInto(dst io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}

// dirBaseName returns the name to use for a directory being archived,
// stripping any trailing slash first ("photos/" and "photos" both give
// "photos").
func dirBaseName(path string) string {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if base == "." || base == "/" {
		return "archive"
	}
	return base
}
