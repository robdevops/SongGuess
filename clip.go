package main

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"time"
)

// clipLength is how much of a preview the game plays.
const clipLength = 5 * time.Second

// errNotMP3 refuses audio that isn't MPEG audio with enough good frames.
var errNotMP3 = errors.New("not an MP3 long enough to clip")

// frame is one MPEG audio frame's place in a file, and how long it plays.
type frame struct {
	start, size int
	dur         time.Duration
}

// Bitrates in kbps, by MPEG version (1, or 2 and 2.5) and layer, indexed
// by the header's 4-bit bitrate field; 0 is free or bad.
var bitrates = map[[2]int][16]int{
	{1, 1}: {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
	{1, 2}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
	{1, 3}: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	{2, 1}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	{2, 2}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	{2, 3}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
}

// sampleRates in Hz by the header's version bits (0: 2.5, 2: 2, 3: 1) and
// its 2-bit rate field.
var sampleRates = map[int][3]int{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}

// parseFrame reads the frame header at b's start: its size in bytes and
// how long it plays, or ok false if b doesn't start with one.
func parseFrame(b []byte) (size int, dur time.Duration, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0, 0, false
	}
	verBits, layerBits := int(b[1]>>3&3), int(b[1]>>1&3)
	rates, known := sampleRates[verBits]
	if !known || layerBits == 0 {
		return 0, 0, false
	}
	layer := 4 - layerBits
	ver := 1
	if verBits != 3 {
		ver = 2
	}
	br := bitrates[[2]int{ver, layer}][b[2]>>4] * 1000
	ri := int(b[2] >> 2 & 3)
	if br == 0 || ri == 3 {
		return 0, 0, false
	}
	sr := rates[ri]
	pad := int(b[2] >> 1 & 1)
	var samples int
	switch {
	case layer == 1:
		size, samples = (12*br/sr+pad)*4, 384
	case layer == 2 || ver == 1:
		size, samples = 144*br/sr+pad, 1152
	default: // layer III, MPEG 2 and 2.5
		size, samples = 72*br/sr+pad, 576
	}
	return size, time.Duration(samples) * time.Second / time.Duration(sr), size > 4
}

// stripTags is audio without its ID3v2 tag at the start and ID3v1 tag at
// the end, which can name the song.
func stripTags(b []byte) []byte {
	if len(b) >= 10 && string(b[:3]) == "ID3" {
		n := int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		n += 10
		if b[5]&0x10 != 0 { // a footer
			n += 10
		}
		b = b[min(n, len(b)):]
	}
	if len(b) >= 128 && string(b[len(b)-128:len(b)-125]) == "TAG" {
		b = b[:len(b)-128]
	}
	return b
}

// frames are the MPEG audio frames in b, each followed by another or the
// end, from the first one found; a Xing or Info frame, which only
// describes the file, is left out.
func frames(b []byte) []frame {
	var out []frame
	i := bytes.IndexByte(b, 0xFF)
	for i >= 0 && i < len(b) {
		size, dur, ok := parseFrame(b[i:])
		if !ok || i+size > len(b) {
			if len(out) > 0 {
				break // the audio ends, or something else follows it
			}
			j := bytes.IndexByte(b[i+1:], 0xFF) // not yet in sync: look further
			if j < 0 {
				break
			}
			i += 1 + j
			continue
		}
		f := frame{start: i, size: size, dur: dur}
		if len(out) == 0 && (bytes.Contains(b[i:i+min(size, 64)], []byte("Xing")) || bytes.Contains(b[i:i+min(size, 64)], []byte("Info"))) {
			i += size // describes the file: leave it out
			continue
		}
		out = append(out, f)
		i += size
	}
	return out
}

// clip is a random clipLength of audio, an MP3, cut at frame edges and
// without tags. The first frame of the cut is dropped, as it may lean on
// audio before it.
func clip(audio []byte, r *rand.Rand) ([]byte, error) {
	b := stripTags(audio)
	fs := frames(b)
	n := 0 // frames in a clip
	var total time.Duration
	for n < len(fs) && total < clipLength {
		total += fs[n].dur
		n++
	}
	if total < clipLength || len(fs) < n+1 {
		return nil, errNotMP3
	}
	start := 1 + r.IntN(len(fs)-n) // after one frame to drop
	first, last := fs[start], fs[start+n-1]
	return bytes.Clone(b[first.start : last.start+last.size]), nil
}
