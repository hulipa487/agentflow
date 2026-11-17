package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxLinkHops bounds symlink resolution while unpacking, so a loop of links
// in a hostile layer ends in an error instead of a hang.
const maxLinkHops = 40

// whiteout names, from the OCI image layer spec: a layer deletes a path of a
// lower layer by shipping an empty file with this prefix, and replaces a whole
// directory by shipping the opaque marker inside it.
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// errLayerTooBig stops an image that unpacks past the size it is allowed.
var errLayerTooBig = errors.New("the image unpacks to more than the allowed size")

// applyLayer unpacks one image layer over root, the way a container runtime
// would, with the differences an unprivileged engine needs: ownership is not
// restored (everything belongs to the engine's user, which is uid 0 inside a
// rootless sandbox anyway), device nodes are skipped (the sandbox has its own
// /dev), setuid bits are dropped, and every directory stays writable by its
// owner so the tree can be removed again.
//
// A layer is data from the network. No entry may land outside root, whatever
// its name says and whatever links earlier entries created: every path is
// resolved component by component inside root before it is touched.
func applyLayer(root string, layer io.Reader, budget *int64) error {
	br := bufio.NewReader(layer)
	magic, _ := br.Peek(4)
	var src io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("layer: %w", err)
		}
		defer gz.Close()
		src = gz
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return errors.New("layer is zstd-compressed, which this build cannot unpack; use an image with gzip layers")
	}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("layer: %w", err)
		}
		name := path.Clean("/" + hdr.Name)[1:]
		if name == "" {
			continue // the root directory itself
		}
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")

		parent, err := resolveIn(root, dir)
		if err != nil {
			return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
		}

		if base == whiteoutOpaque {
			if err := emptyDir(parent); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			if err := os.RemoveAll(filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			continue
		}

		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
		}
		target := filepath.Join(parent, base)
		mode := os.FileMode(hdr.Mode) & 0o777

		switch hdr.Typeflag {
		case tar.TypeDir:
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				if err := os.Remove(target); err != nil {
					return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			if err := os.Chmod(target, mode|0o700); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}

		case tar.TypeReg:
			*budget -= hdr.Size
			if *budget < 0 {
				return errLayerTooBig
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode|0o600)
			if err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			_, err = io.Copy(f, io.LimitReader(tr, hdr.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}

		case tar.TypeSymlink:
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			// The link text is stored as it is: it is followed inside the
			// sandbox, where / is this tree. Unpacking never follows it
			// blindly — resolveIn does, and keeps the result inside root.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}

		case tar.TypeLink:
			old, err := resolveIn(root, path.Clean("/" + hdr.Linkname)[1:])
			if err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}
			if err := os.Link(old, target); err != nil {
				return fmt.Errorf("layer entry %q: %w", hdr.Name, err)
			}

		default:
			// Devices, FIFOs and the rest: nothing a job needs from an image.
		}
	}
}

// resolveIn maps a slash path inside an image to a host path under root,
// following the symlinks already unpacked as the sandbox would: an absolute
// link target starts again at root, and ".." never climbs above it. The
// result is always inside root.
func resolveIn(root, name string) (string, error) {
	hops := 0
	resolved := root
	pending := strings.Split(name, "/")
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if resolved != root {
				resolved = filepath.Dir(resolved)
			}
			continue
		}
		next := filepath.Join(resolved, part)
		st, err := os.Lstat(next)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			// Missing (to be created) or a real entry: descend.
			resolved = next
			continue
		}
		if hops++; hops > maxLinkHops {
			return "", fmt.Errorf("too many symbolic links in %q", name)
		}
		link, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if path.IsAbs(link) {
			resolved = root
		}
		pending = append(strings.Split(link, "/"), pending...)
	}
	return resolved, nil
}

// emptyDir removes everything inside a directory, keeping the directory. A
// directory that is not there yet has nothing to remove.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
