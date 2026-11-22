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
		c, err := clip(src, rand.New(rand.NewPCG(seed, 1)))
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
		if _, err := clip(b, rand.New(rand.NewPCG(1, 1))); err == nil {
			t.Errorf("%s clipped", name)
		}
	}
}
