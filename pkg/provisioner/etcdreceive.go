package provisioner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// A snapshot streamed to a node ends on the hex SHA-256 of what came before it, so the receiver
// knows the stream came whole before the restore stops etcd.
const receiveTrailer = sha256.Size * 2

// errStreamCut says a stream ended before its checksum, or with another one.
var errStreamCut = errors.New("the snapshot arrived cut short or changed: its SHA-256 does not match")

// ReceiveRoom refuses a snapshot the node's disk at dir, with free bytes left, has no room for.
func ReceiveRoom(dir string, need int64, free uint64) error {
	if need > 0 && free < uint64(need) {
		return fmt.Errorf("the node has %s free at %s, and restoring this snapshot needs %s. Free space on the node, or restore by hand", formatBytes(int64(free)), dir, formatBytes(need)) //nolint:gosec // free is far below the int64 range
	}
	return nil
}

// SendEtcdSnapshot writes body to w, then its checksum.
func SendEtcdSnapshot(w io.Writer, body io.Reader) error {
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, sum), body); err != nil {
		return fmt.Errorf("streaming the snapshot: %w", err)
	}
	if _, err := io.WriteString(w, hex.EncodeToString(sum.Sum(nil))); err != nil {
		return fmt.Errorf("streaming the snapshot: %w", err)
	}
	return nil
}

// ReceiveEtcdSnapshot writes a stream SendEtcdSnapshot made to out, all but its checksum, and tells
// how many bytes it wrote. It fails unless the checksum matches.
func ReceiveEtcdSnapshot(in io.Reader, out io.Writer) (int64, error) {
	sum := sha256.New()
	w := io.MultiWriter(out, sum)
	buf := make([]byte, 0, 64<<10+receiveTrailer)
	var written int64
	for {
		n, err := in.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if keep := len(buf) - receiveTrailer; keep > 0 && (len(buf) == cap(buf) || err != nil) {
			if _, werr := w.Write(buf[:keep]); werr != nil {
				return written, fmt.Errorf("writing the snapshot: %w", werr)
			}
			written += int64(keep)
			buf = append(buf[:0], buf[keep:]...)
		}
		switch {
		case errors.Is(err, io.EOF):
			want := make([]byte, sha256.Size)
			if len(buf) != receiveTrailer {
				return written, errStreamCut
			}
			if _, derr := hex.Decode(want, buf); derr != nil || !bytes.Equal(want, sum.Sum(nil)) {
				return written, errStreamCut
			}
			return written, nil
		case err != nil:
			return written, fmt.Errorf("reading the snapshot: %w", err)
		}
	}
}
