package audio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PCM fixtures are generated in memory, so tests ship no copyrighted media.
func wav(samples int) []byte {
	b := make([]byte, 44+samples*2)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(samples*2))
	for i := range samples {
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(i%1000))
	}
	return b
}
func verifier(t *testing.T) Verifier {
	t.Helper()
	a, e := exec.LookPath("ffmpeg")
	if e != nil {
		if os.Getenv("REQUIRE_FFMPEG") == "1" {
			t.Fatal(e)
		}
		t.Skip("ffmpeg not installed")
	}
	b, e := exec.LookPath("ffprobe")
	if e != nil {
		t.Fatal(e)
	}
	return Verifier{FFmpeg: a, FFprobe: b, TempDir: t.TempDir()}
}
func TestPCMHashStableAcrossLosslessContainers(t *testing.T) {
	v := verifier(t)
	ctx := context.Background()
	input := wav(800)
	raw, e := v.Verify(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	var a Result
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	if a.Codec != "pcm_s16le" || a.Bits != 16 || a.Channels != 1 || a.SampleRate != "8000" || len(a.PCMHash) != 64 {
		t.Fatal(a)
	}
	source := filepath.Join(t.TempDir(), "input.wav")
	target := filepath.Join(t.TempDir(), "output.flac")
	os.WriteFile(source, input, 0600)
	if out, e := exec.Command(v.FFmpeg, "-v", "error", "-i", source, target).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, out)
	}
	flac, e := os.ReadFile(target)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = v.Verify(ctx, flac)
	if e != nil {
		t.Fatal(e)
	}
	var b Result
	json.Unmarshal(raw, &b)
	if a.PCMHash != b.PCMHash || b.Codec != "flac" {
		t.Fatalf("PCM differs: %+v %+v", a, b)
	}
	files, e := os.ReadDir(v.TempDir)
	if e != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v %v", files, e)
	}
}
func TestRejectInvalidAndEmptyAudio(t *testing.T) {
	v := verifier(t)
	for name, input := range map[string][]byte{"garbage": []byte("not audio"), "empty PCM": wav(0), "truncated": wav(800)[:50]} {
		t.Run(name, func(t *testing.T) {
			if _, e := v.Verify(context.Background(), input); e == nil {
				t.Fatal("invalid audio accepted")
			}
		})
	}
}
func TestRejectLossyCodec(t *testing.T) {
	v := verifier(t)
	source := filepath.Join(t.TempDir(), "source.wav")
	target := filepath.Join(t.TempDir(), "lossy.m4a")
	os.WriteFile(source, wav(800), 0600)
	if out, e := exec.Command(v.FFmpeg, "-v", "error", "-i", source, "-c:a", "aac", target).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, out)
	}
	b, e := os.ReadFile(target)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Verify(context.Background(), b); e == nil {
		t.Fatal("lossy AAC accepted")
	}
}
func TestCanceledContext(t *testing.T) {
	v := verifier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := v.Verify(ctx, wav(800)); e == nil {
		t.Fatal("canceled operation succeeded")
	}
}

func TestALACAndAIFF(t *testing.T) {
	v := verifier(t)
	source := filepath.Join(t.TempDir(), "source.wav")
	if e := os.WriteFile(source, wav(800), 0600); e != nil {
		t.Fatal(e)
	}
	base, e := v.Verify(context.Background(), wav(800))
	if e != nil {
		t.Fatal(e)
	}
	var expected Result
	if e = json.Unmarshal(base, &expected); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		name, codec string
		args        []string
		bits        int
	}{
		{"test.m4a", "alac", []string{"-c:a", "alac", "-sample_fmt", "s32"}, 24},
		{"test.aiff", "pcm_s24be", []string{"-c:a", "pcm_s24be"}, 24},
		{"test.flac", "flac", []string{"-c:a", "flac", "-sample_fmt", "s32"}, 24},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), c.name)
			args := append([]string{"-v", "error", "-i", source}, c.args...)
			args = append(args, target)
			if out, e := exec.Command(v.FFmpeg, args...).CombinedOutput(); e != nil {
				t.Fatalf("%v %s", e, out)
			}
			b, e := os.ReadFile(target)
			if e != nil {
				t.Fatal(e)
			}
			raw, e := v.Verify(context.Background(), b)
			if e != nil {
				t.Fatal(e)
			}
			var got Result
			if e = json.Unmarshal(raw, &got); e != nil {
				t.Fatal(e)
			}
			if got.Codec != c.codec || got.Bits != c.bits || got.PCMHash != expected.PCMHash {
				t.Fatalf("unexpected result: %+v", got)
			}
		})
	}
}

// Wrapping a reader hides WriterTo, exercising io.Copy's destination fast path.
// Embedding bytes.Buffer here would accidentally promote its unbounded ReadFrom.
func TestSubprocessOutputRemainsBounded(t *testing.T) {
	b := &limitedOutput{limit: 16}
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 1024))}
	n, e := io.Copy(b, source)
	if e != nil || n != 1024 || len(b.Bytes()) != 16 {
		t.Fatalf("read %d bytes, stored %d: %v", n, len(b.Bytes()), e)
	}
}
