package gateway

import (
	"encoding/base64"
	"os"
	"path/filepath"

	"github.com/bam/claude_spawner/server/internal/session"
)

// Cap a single file transfer (either direction) to bound memory: the bytes are held
// whole in RAM and carried base64-encoded in one WebSocket message. Source files and
// configs — the intended payloads — sit far below this.
const maxTransferBytes = 64 << 20 // 64 MiB

// doUpload writes an uploaded file to the target host. path is the destination
// *directory* (defaults to the host's root when empty is passed through unclean),
// name is the filename, and content is the file's base64 bytes. On success it echoes
// `file_saved` with the file's absolute path so the app can prefill the message box.
func (c *conn) doUpload(dir, name, host, content string) {
	if name == "" {
		c.fail("bad_path", "upload needs a filename")
		return
	}
	if dir == "" {
		c.fail("bad_path", "upload needs a destination directory")
		return
	}
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		c.fail("bad_message", "upload content is not valid base64")
		return
	}
	if len(data) > maxTransferBytes {
		c.fail("file_too_large", "file exceeds the transfer size limit")
		return
	}
	// The destination is a single file inside dir; join defends against a name that
	// smuggles path separators by taking only its base.
	dest := filepath.Join(filepath.Clean(dir), filepath.Base(name))
	if err := c.writeFile(host, dest, data); err != nil {
		c.fail("transfer_failed", err.Error())
		return
	}
	c.send(msgFileSaved(dest))
}

// doDownload reads a file off the source host and returns it to the app as
// `file_data` (base64). path is the absolute file path; host is the machine it lives
// on ("" = local).
func (c *conn) doDownload(path, host string) {
	if path == "" {
		c.fail("bad_path", "download needs a file path")
		return
	}
	clean := filepath.Clean(path)
	data, err := c.readFile(host, clean)
	if err != nil {
		c.fail("transfer_failed", err.Error())
		return
	}
	if len(data) > maxTransferBytes {
		c.fail("file_too_large", "file exceeds the transfer size limit")
		return
	}
	c.send(msgFileData(filepath.Base(clean), clean, base64.StdEncoding.EncodeToString(data)))
}

// readFile returns path's bytes from the given execution location: over SSH for a
// host target when SSH-native is enabled, else the server's own filesystem.
func (c *conn) readFile(host, path string) ([]byte, error) {
	if host == "" {
		host = session.LocalHost
	}
	if c.srv.ssh != nil {
		return c.srv.ssh.ReadFile(c.ctx, host, path)
	}
	return os.ReadFile(path)
}

// writeFile writes data to path in the given execution location — over SSH for a
// host target when SSH-native is enabled, else the server's own filesystem.
func (c *conn) writeFile(host, path string, data []byte) error {
	if host == "" {
		host = session.LocalHost
	}
	if c.srv.ssh != nil {
		return c.srv.ssh.WriteFile(c.ctx, host, path, data)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
