package main

import (
	"bytes"
	"math/rand/v2"
	"testing"
	"time"
)

// mp3 is a synthetic MP3: an ID3v2 tag naming the song, a Xing frame, n
// frames of MPEG 1 layer III at 128 kbps, 44.1 kHz (417 or 418 bytes, by
// padding), and an ID3v1 tag.
func mp3(n int) []byte {
	var b bytes.Buffer
	tag := []byte("TIT2 Never Gonna Give You Up")
	b.Write([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, byte(len(tag))})
	b.Write(tag)
	frameOf := func(pad bool, fill byte) []byte {
		h := []byte{0xFF, 0xFB, 0x90, 0x00}
		size := 417
		if pad {
			h[2] |= 2
			size = 418
		}
		f := bytes.Repeat([]byte{fill}, size)
		copy(f, h)
		return f
	}
	x := frameOf(false, 0)
	copy(x[36:], "Xing")
	b.Write(x)
	for i := range n {
		b.Write(frameOf(i%3 == 0, byte(1+i%200)))
	}
	b.WriteString("TAG")
	b.Write(make([]byte, 125))
	return b.Bytes()
}

func TestClip(t *testing.T) {
	src := mp3(1150) // about 30s
	starts := map[byte]bool{}
	for seed := range uint64(20) {
		c, err := clip(src, clipLength, rand.New(rand.NewPCG(seed, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(c, []byte("Never Gonna")) || bytes.Contains(c, []byte("TAG")) || bytes.Contains(c, []byte("Xing")) {
			t.Fatal("a tag or the Xing frame is in the clip")
		}
		var total time.Duration
		for i := 0; i < len(c); {
			size, dur, ok := parseFrame(c[i:])
			if !ok {
				t.Fatalf("no frame at %d", i)
			}
			i += size
			total += dur
		}
		if total < clipLength || total > clipLength+30*time.Millisecond {
			t.Errorf("clip of %s", total)
		}
		starts[c[4]] = true
	}
	if len(starts) < 5 {
		t.Errorf("the clip starts at only %d places", len(starts))
	}
	for name, b := range map[string][]byte{"junk": bytes.Repeat([]byte("junk"), 5000), "short": mp3(100), "empty": nil} {
		if _, err := clip(b, clipLength, rand.New(rand.NewPCG(1, 1))); err == nil {
			t.Errorf("%s clipped", name)
		}
	}
}

// /song 10s asks for a longer clip, which is as long; a 25s clip fits a
// 30s preview, and a length that doesn't fit is refused.
func TestClipLength(t *testing.T) {
	src := mp3(1150)
	for _, want := range []time.Duration{time.Second, 10 * time.Second, maxClip} {
		c, err := clip(src, want, rand.New(rand.NewPCG(7, 1)))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		var total time.Duration
		for i := 0; i < len(c); {
			size, dur, _ := parseFrame(c[i:])
			i, total = i+size, total+dur
		}
		if total < want || total > want+30*time.Millisecond {
			t.Errorf("asked %s, got %s", want, total)
		}
	}
	if _, err := clip(src, 31*time.Second, rand.New(rand.NewPCG(7, 1))); err == nil {
		t.Error("a clip longer than the preview")
	}
	for s, want := range map[string]time.Duration{"10s": 10 * time.Second, "25S": maxClip, "1s": time.Second} {
		if d, err := clipArg(s); err != nil || d != want {
			t.Errorf("clipArg(%q) = %s, %v", s, d, err)
		}
	}
	for _, s := range []string{"0s", "26s", "2.5s", "1m", "ten", ""} {
		if _, err := clipArg(s); err == nil {
			t.Errorf("clipArg(%q) passed", s)
		}
	}
}
