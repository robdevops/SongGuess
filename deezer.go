package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// track is one song from a Deezer chart, as the game keeps it.
type track struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`           // as Deezer gives it, versions and all
	Short   string `json:"short,omitempty"` // title_short: without the version
	Artist  string `json:"artist"`
	Cover   string `json:"cover,omitempty"` // album.cover_medium, a JPEG
	Preview string `json:"preview"`         // a 30-second MP3
}

// genre is one of Deezer's genres, whose chart /song <genre> plays.
type genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// The limits of what the game fetches.
const (
	maxAPI         = 1 << 20   // a genre list or chart
	maxPreview     = 2 << 20   // a preview MP3
	maxCover       = 300 << 10 // an album cover
	apiTimeout     = 10 * time.Second
	previewTimeout = 15 * time.Second
	coverTimeout   = 5 * time.Second
	genresFor      = 24 * time.Hour // how long the genre list is kept
	chartSize      = 100
)

// userAgent names the game to Deezer, as it asks of API clients.
const userAgent = "chat-plugin-songs/1 (+https://github.com/robdevops/chat-plugin-songs)"

// deezer is Deezer's public API, which needs no key: its genres, their
// charts, and the previews and covers the charts link to.
type deezer struct {
	base      string // SONGS_DEEZER
	client    *http.Client
	allowHTTP bool // fetch http:// previews and covers too, when the API itself is http (a test's)

	mu       sync.Mutex
	genres   []genre
	genresAt time.Time
}

func newDeezer(base string, client *http.Client) *deezer {
	d := &deezer{base: base, allowHTTP: strings.HasPrefix(base, "http://")}
	c := *client // don't change the shared client
	// Every redirect must be https too, as get checks of the first URL.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && !(d.allowHTTP && req.URL.Scheme == "http") {
			return errors.New("redirect to a non-https URL")
		}
		return nil
	}
	d.client = &c
	return d
}

// errDeezer is a failure of Deezer's, as players are told of it.
var errDeezer = errors.New("deezer failed")

// get fetches rawURL, at most limit bytes. Its errors never hold the URL,
// so the log never names a song.
func (d *deezer) get(ctx context.Context, what, rawURL string, limit int64, timeout time.Duration) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || !(u.Scheme == "https" || d.allowHTTP && u.Scheme == "http") {
		return nil, fmt.Errorf("%s: not an https URL: %w", what, errDeezer)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: bad request: %w", what, errDeezer)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := d.client.Do(req)
	if err != nil {
		var ue *url.Error
		switch {
		case ctx.Err() != nil || errors.As(err, &ue) && ue.Timeout():
			return nil, fmt.Errorf("%s: timed out: %w", what, errDeezer)
		default:
			return nil, fmt.Errorf("%s: can't connect: %w", what, errDeezer)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d: %w", what, resp.StatusCode, errDeezer)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%s: over %d bytes: %w", what, limit, errDeezer)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: cut off: %w", what, errDeezer)
	case int64(len(body)) > limit:
		return nil, fmt.Errorf("%s: over %d bytes: %w", what, limit, errDeezer)
	}
	return body, nil
}

// getJSON fetches an API path into v. Deezer answers some failures, such
// as its quota, with 200 and an error object.
func (d *deezer) getJSON(ctx context.Context, what, path string, v any) error {
	body, err := d.get(ctx, what, d.base+path, maxAPI, apiTimeout)
	if err != nil {
		return err
	}
	var e struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil {
		return fmt.Errorf("%s: Deezer's error %d: %w", what, e.Error.Code, errDeezer)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: bad JSON: %w", what, errDeezer)
	}
	return nil
}

// Genres is Deezer's genre list, kept for a day.
func (d *deezer) Genres(ctx context.Context) ([]genre, error) {
	d.mu.Lock()
	if d.genres != nil && time.Since(d.genresAt) < genresFor {
		defer d.mu.Unlock()
		return d.genres, nil
	}
	d.mu.Unlock()
	var r struct {
		Data []genre `json:"data"`
	}
	if err := d.getJSON(ctx, "the genres", "/genre", &r); err != nil {
		return nil, err
	}
	var list []genre
	for _, g := range r.Data {
		if g.ID >= 0 && strings.TrimSpace(g.Name) != "" {
			list = append(list, genre{g.ID, clean(g.Name)})
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("the genres: none: %w", errDeezer)
	}
	d.mu.Lock()
	d.genres, d.genresAt = list, time.Now()
	d.mu.Unlock()
	return list, nil
}

// Chart is the top tracks of a genre's chart (0 is all genres) that have a
// preview.
func (d *deezer) Chart(ctx context.Context, genreID int) ([]track, error) {
	return d.tracks(ctx, "the chart", "/chart/"+strconv.Itoa(genreID)+"/tracks?limit="+strconv.Itoa(chartSize))
}

// Playlist is a playlist's tracks with a preview, in its order.
func (d *deezer) Playlist(ctx context.Context, id int64) ([]track, error) {
	return d.tracks(ctx, "a playlist", "/playlist/"+strconv.FormatInt(id, 10)+"/tracks?limit="+strconv.Itoa(chartSize))
}

// tracks are the tracks with a preview that path at the API lists, as a
// chart or a playlist does.
func (d *deezer) tracks(ctx context.Context, what, path string) ([]track, error) {
	var r struct {
		Data []struct {
			ID         int64  `json:"id"`
			Title      string `json:"title"`
			TitleShort string `json:"title_short"`
			Preview    string `json:"preview"`
			Artist     struct {
				Name string `json:"name"`
			} `json:"artist"`
			Album struct {
				CoverMedium string `json:"cover_medium"`
			} `json:"album"`
		} `json:"data"`
	}
	if err := d.getJSON(ctx, what, path, &r); err != nil {
		return nil, err
	}
	var out []track
	for _, t := range r.Data {
		// Square brackets, which clean takes out, are a version's as
		// parentheses are: "Song [Remastered]".
		brackets := strings.NewReplacer("[", "(", "]", ")")
		title, artist := clean(brackets.Replace(t.Title)), clean(t.Artist.Name)
		if t.ID <= 0 || t.Preview == "" || title == "" || artist == "" {
			continue
		}
		out = append(out, track{ID: t.ID, Title: clamp(title, 200), Short: clamp(clean(brackets.Replace(t.TitleShort)), 200), Artist: clamp(artist, 200), Cover: t.Album.CoverMedium, Preview: t.Preview})
	}
	return out, nil
}

// Preview is a track's 30-second preview, an MP3.
func (d *deezer) Preview(ctx context.Context, t track) ([]byte, error) {
	return d.get(ctx, "a preview", t.Preview, maxPreview, previewTimeout)
}

// Cover is a track's album cover, a JPEG.
func (d *deezer) Cover(ctx context.Context, t track) ([]byte, error) {
	if t.Cover == "" {
		return nil, fmt.Errorf("a cover: none: %w", errDeezer)
	}
	b, err := d.get(ctx, "a cover", t.Cover, maxCover, coverTimeout)
	if err == nil && (len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF) {
		err = fmt.Errorf("a cover: not a JPEG: %w", errDeezer)
	}
	return b, err
}

// decade is a decade /song can play: its label, and the "Hits" playlist
// Deezer's editors keep for it.
type decade struct {
	Label    string
	Playlist int64
}

// decades are Deezer's editors' playlists of each decade's hits, as found
// in November 2026. If one is gone, the search finds its successor.
var decades = []decade{
	{"60s", 620264073}, {"70s", 1470022445}, {"80s", 867825522},
	{"90s", 878989033}, {"2000s", 248297032}, {"2010s", 14917741483},
}

// findDecade is the decade name names, written with its century or an
// apostrophe, so it is never a length: 1980s, 1980's, 80's or '80s, or a
// word such as eighties; ok false for anything else, 80s included.
func findDecade(name string) (decade, bool) {
	n := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "’", "'"))
	words := map[string]string{"sixties": "1960s", "seventies": "1970s", "eighties": "1980s", "nineties": "1990s", "noughties": "2000s"}
	if w, ok := words[n]; ok {
		n = w
	}
	switch {
	case len(n) == 6 && n[4:] == "'s": // 1980's
		n = n[:4] + "s"
	case len(n) == 4 && (n[2:] == "'s" || n[0] == '\''): // 80's, '80s
		n = strings.ReplaceAll(n, "'", "")
		century := "19"
		if n[0] == '0' || n[0] == '1' {
			century = "20"
		}
		n = century + n
	}
	if len(n) != 5 || n[4] != 's' || n[3] != '0' || strings.Trim(n[:4], "0123456789") != "" {
		return decade{}, false
	}
	for _, d := range decades {
		if d.full() == n {
			return d, true
		}
	}
	return decade{}, false
}

// full is the decade with its century: 1980s.
func (d decade) full() string {
	if strings.HasPrefix(d.Label, "20") {
		return d.Label
	}
	return "19" + d.Label
}

// Decade is a decade's hits: its editors' playlist, or, if that is gone
// or empty, the playlist a search for "<decade> Hits" finds that Deezer's
// editors made (their 2000s and 2010s are "00s Hits" and "10s Hits").
func (d *deezer) Decade(ctx context.Context, dec decade) ([]track, error) {
	list, err := d.Playlist(ctx, dec.Playlist)
	if err == nil && len(list) > 0 {
		return list, nil
	}
	var r struct {
		Data []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			User  struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"data"`
	}
	short := strings.TrimPrefix(strings.TrimPrefix(dec.Label, "20"), "19") // Deezer's titles: "00s Hits"
	if err := d.getJSON(ctx, "a playlist search", "/search/playlist?q="+url.QueryEscape(short+" hits")+"&limit=25", &r); err != nil {
		return nil, err
	}
	var pick int64
	for _, p := range r.Data { // the editors' own "80s Hits" first, then any of theirs
		if !strings.Contains(p.User.Name, "Deezer") {
			continue
		}
		if normalize(p.Title) == normalize(short+" hits") {
			pick = p.ID
			break
		}
		if pick == 0 {
			pick = p.ID
		}
	}
	if pick == 0 {
		return nil, fmt.Errorf("a decade's playlist: none found: %w", errDeezer)
	}
	return d.Playlist(ctx, pick)
}

// findGenre is the genre called name, in any case and with or without its
// punctuation ("hip hop" and "rap" are both Rap/Hip Hop's), or ok false.
func findGenre(genres []genre, name string) (genre, bool) {
	want := normalize(name)
	if want == "" {
		return genre{}, false
	}
	if a, ok := genreAliases[strings.ReplaceAll(want, " ", "")]; ok { // what people type for names Deezer writes otherwise
		want = normalize(a)
	}
	squash := func(s string) string { return strings.ReplaceAll(s, " ", "") }
	for _, g := range genres {
		if n := normalize(g.Name); n == want || squash(n) == squash(want) {
			return g, true
		}
	}
	for _, g := range genres {
		for _, part := range strings.Split(g.Name, "/") {
			if n := normalize(part); n != "" && (n == want || squash(n) == squash(want)) {
				return g, true
			}
		}
	}
	return genre{}, false
}

// genreAliases are genre names as people type them, by their normalised
// letters, and Deezer's names for them.
var genreAliases = map[string]string{"rnb": "R&B", "randb": "R&B", "hiphop": "Hip Hop", "edm": "Electro", "electronic": "Electro"}

// clean is s as a post may carry it: on one line, with Markdown's
// characters and braces taken out, so a title can't style a post, link, or
// name anyone.
func clean(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_`~[]{}<>|\\", r) {
			return -1
		}
		return r
	}, s)), " ")
}
