package main

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// config is the game's settings, every one from a SONGS_ variable, the
// only ones signalbot passes an addon called songs.
type config struct {
	StateDir string        // SONGS_STATE_DIR: where songs.json and the log are kept
	Songs    int           // SONGS_SONGS: a game's default number of songs
	Time     time.Duration // SONGS_TIME: the time to answer each song
	MemLimit int64         // SONGS_MEMLIMIT, in bytes
	Deezer   string        // SONGS_DEEZER: the Deezer API's base URL, without a final /
	gap      time.Duration // between a song's reveal and the next song: gapTime, not a setting; 0 in most tests
}

// The limits of a game, and the defaults of the settings.
const (
	minSongs        = 1
	maxSongs        = 10
	defaultSongs    = 5
	minTime         = 10 * time.Second
	maxTime         = 2 * time.Minute
	defaultTime     = 30 * time.Second
	defaultMemLimit = "64MiB"
	deezerURL       = "https://api.deezer.com"
)

// loadConfig reads the settings through getenv (os.Getenv), failing with a
// message that names the setting.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		StateDir: strings.TrimSpace(getenv("SONGS_STATE_DIR")),
		Songs:    defaultSongs,
		Time:     defaultTime,
		Deezer:   strings.TrimRight(cmp.Or(strings.TrimSpace(getenv("SONGS_DEEZER")), deezerURL), "/"),
		gap:      gapTime,
	}
	var errs []error
	if c.StateDir == "" {
		errs = append(errs, errors.New("SONGS_STATE_DIR is not set: signalbot sets it for an addon called songs from its own state directory, or set it to a directory the game may create or write"))
	}
	var err error
	if c.MemLimit, err = parseSize(cmp.Or(strings.TrimSpace(getenv("SONGS_MEMLIMIT")), defaultMemLimit)); err != nil {
		errs = append(errs, fmt.Errorf("SONGS_MEMLIMIT: %v", err))
	}
	if v := strings.TrimSpace(getenv("SONGS_SONGS")); v != "" {
		if c.Songs, err = songCount(v); err != nil {
			errs = append(errs, fmt.Errorf("SONGS_SONGS: %v", err))
		}
	}
	if v := strings.TrimSpace(getenv("SONGS_TIME")); v != "" {
		if c.Time, err = songTime(v); err != nil {
			errs = append(errs, fmt.Errorf("SONGS_TIME: %v", err))
		}
	}
	if u, err := url.Parse(c.Deezer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, fmt.Errorf("SONGS_DEEZER is %q: use the API's base URL, such as %s", clamp(c.Deezer, 80), deezerURL))
	}
	return c, errors.Join(errs...)
}

// songCount reads a number of songs, 1 to 10.
func songCount(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < minSongs || n > maxSongs {
		return 0, fmt.Errorf("%q isn't a number of songs: use 1 to 10", clamp(s, 40))
	}
	return n, nil
}

// songTime reads the time to answer each song, such as 45s or 2m, from
// 10s to 2m.
func songTime(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d < minTime || d > maxTime {
		return 0, fmt.Errorf("%q isn't a time to answer each song: use 10s to 2m, such as 45s", clamp(s, 40))
	}
	return d, nil
}

// sizeUnits are the suffixes parseSize takes, as GOMEMLIMIT does.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

// parseSize reads a number of bytes written as GOMEMLIMIT is, such as
// 64MiB or 1GiB.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	num := s
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, mult = n, u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n < 0 || n*float64(mult) >= math.MaxInt64 {
		return 0, fmt.Errorf("%q isn't a size: use a number of bytes with B, KiB, MiB or GiB, such as 64MiB", s)
	}
	if n*float64(mult) < 16<<20 {
		return 0, fmt.Errorf("%q is too small: the game needs at least 16MiB", s)
	}
	return int64(n * float64(mult)), nil
}

// checkDir creates dir, for the game's user alone, if it isn't there, and
// fails unless it can be listed and written, leaving nothing.
func checkDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("SONGS_STATE_DIR %s can't be created (%v): create it, writable by the bot's user", dir, err)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("SONGS_STATE_DIR %s can't be read (%v): make it readable by the bot's user", dir, err)
	}
	f, err := os.CreateTemp(dir, ".check-*")
	if err == nil {
		f.Close()
		err = os.Remove(f.Name())
	}
	if err != nil {
		return fmt.Errorf("SONGS_STATE_DIR %s can't be written (%v): make it writable by the bot's user", dir, err)
	}
	return nil
}

// clamp cuts s to max runes, marking a cut with "…".
func clamp(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
