// Package audio verifies decodability and hashes a defined PCM representation.
package audio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Verifier struct{ FFmpeg, FFprobe, TempDir string }
type Result struct {
	Codec      string `json:"codec"`
	SampleRate string `json:"sample_rate_hz"`
	Channels   int    `json:"channels"`
	Bits       int    `json:"bits_per_sample"`
	PCMHash    string `json:"pcm_s32le_sha256"`
}

// A bounded writer keeps malformed metadata and subprocess diagnostics from
// consuming unbounded memory. The process still receives successful writes.
type limitedOutput struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.buf.Len()
	if left > 0 {
		b.buf.Write(p[:min(left, n)])
	}
	return n, nil
}
func (b *limitedOutput) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedOutput) String() string { return b.buf.String() }

func (v Verifier) Verify(ctx context.Context, input []byte) ([]byte, error) {
	f, err := os.CreateTemp(v.TempDir, "media-job-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(input); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	meta := &limitedOutput{limit: 64 << 10}
	cmd := exec.CommandContext(ctx, v.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "flac,wav,aiff,mov", "-select_streams", "a:0", "-show_entries", "stream=codec_name,sample_rate,channels,bits_per_raw_sample,bits_per_sample", "-of", "json", name)
	cmd.Stdout = meta
	if err = cmd.Run(); err != nil {
		return nil, errors.New("probe failed")
	}
	var probe struct {
		Streams []struct {
			Codec    string `json:"codec_name"`
			Rate     string `json:"sample_rate"`
			Channels int    `json:"channels"`
			Raw      string `json:"bits_per_raw_sample"`
			Bits     int    `json:"bits_per_sample"`
		}
	}
	if err = json.Unmarshal(meta.Bytes(), &probe); err != nil || len(probe.Streams) != 1 {
		return nil, errors.New("audio stream missing")
	}
	s := probe.Streams[0]
	var bits int
	if s.Raw != "" {
		if err = json.Unmarshal([]byte(s.Raw), &bits); err != nil {
			return nil, errors.New("invalid bit depth")
		}
	}
	if bits == 0 {
		bits = s.Bits
	}
	allowed := s.Codec == "flac" || s.Codec == "alac" || strings.HasPrefix(s.Codec, "pcm_s") || strings.HasPrefix(s.Codec, "pcm_u")
	if !allowed || (bits != 8 && bits != 16 && bits != 24 && bits != 32) || s.Channels < 1 {
		return nil, errors.New("unsupported audio format")
	}
	out := &limitedOutput{limit: 256}
	cmd = exec.CommandContext(ctx, v.FFmpeg, "-nostdin", "-hide_banner", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-format_whitelist", "flac,wav,aiff,mov", "-i", name, "-map", "0:a:0", "-vn", "-sn", "-dn", "-c:a", "pcm_s32le", "-f", "hash", "-hash", "sha256", "-")
	cmd.Stdout = out
	if err = cmd.Run(); err != nil {
		return nil, errors.New("decode failed")
	}
	hash := strings.TrimSpace(strings.TrimPrefix(out.String(), "SHA256="))
	decoded, err := hex.DecodeString(hash)
	empty := sha256.Sum256(nil)
	if err != nil || len(decoded) != 32 || bytes.Equal(decoded, empty[:]) {
		return nil, errors.New("invalid or empty PCM digest")
	}
	return json.Marshal(Result{Codec: s.Codec, SampleRate: s.Rate, Channels: s.Channels, Bits: bits, PCMHash: hash})
}

var _ io.Writer = (*limitedOutput)(nil)
