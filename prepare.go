package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/robdevops/signalbot/addon"
)

// The limits of making songs ready: the tracks a game keeps to try, the
// tries for one song, and how long a cover may take.
const (
	poolExtra = 8 // tracks beyond the songs asked for, to replace ones that fail
	maxTries  = 4 // tracks tried for one song before the game gives up on it
)

// errNoSong is a song no track could be made ready for.
var errNoSong = errors.New("no track could be played")

// pick is up to n+poolExtra of chart's tracks not played in chat lately,
// in a random order.
func (s *server) pick(chat string, chart []track, n int) []track {
	s.mu.Lock()
	s.prunePlayed(s.clock.Now())
	recent := map[int64]bool{}
	for _, st := range s.played[chat] {
		recent[st.ID] = true
	}
	s.mu.Unlock()
	var pool []track
	for _, t := range chart {
		if !recent[t.ID] {
			recent[t.ID] = true // and no track twice
			pool = append(pool, t)
		}
	}
	s.randMu.Lock()
	s.rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	s.randMu.Unlock()
	return pool[:min(len(pool), n+poolExtra)]
}

// fetch makes t ready to play: its preview, downloaded and clipped to
// length, and its cover, if there's time and it comes.
func (s *server) fetch(ctx context.Context, t track, length time.Duration) (media, error) {
	audio, err := s.deezer.Preview(ctx, t)
	if err != nil {
		return media{}, err
	}
	s.randMu.Lock()
	c, err := clip(audio, length, s.rand)
	s.randMu.Unlock()
	if err != nil {
		return media{}, err
	}
	m := media{clip: c}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > coverTimeout {
		if cover, err := s.deezer.Cover(ctx, t); err == nil {
			m.cover = cover
		}
	}
	return m, nil
}

// first makes the first of pool's tracks it can ready, trying up to
// maxTries+1 of them until ctx ends; it returns the track, its media, and
// the tracks left.
func (s *server) first(ctx context.Context, pool []track, length time.Duration) (track, media, []track, error) {
	for tries := 0; len(pool) > 0 && tries <= maxTries && ctx.Err() == nil; tries++ {
		t := pool[0]
		pool = pool[1:]
		m, err := s.fetch(ctx, t, length)
		if err == nil {
			return t, m, pool, nil
		}
		s.log.Printf("a track skipped: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return track{}, media{}, pool, err
	}
	return track{}, media{}, pool, errNoSong
}

// prepareNext starts making ready, in the background, the song g plays
// next, unless it is ready, being made ready, or past the game's end, or
// the game's songs are still being chosen.
// s.mu is held.
func (s *server) prepareNext(g *game) {
	k := g.Current
	if g.Post != "" {
		k++
	}
	if g.Preparing || k >= g.Count || s.preparing[g.ID] {
		return
	}
	if _, ok := s.media[g.ID][k]; ok {
		return
	}
	// The song chosen before a restart is tried again first.
	var again []track
	if k < len(g.Songs) {
		again = []track{g.Songs[k]}
	}
	s.preparing[g.ID] = true
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.prepare(g.Chat, g.ID, k, again, g.Clip)
	}()
}

// prepare makes song k of the game id in chat ready, trying the track in
// again, then the game's pool, up to maxTries in all, and posts it if it
// is due. With none, the game plays only the songs before it.
func (s *server) prepare(chat, id string, k int, again []track, length time.Duration) {
	for tries := 0; tries < maxTries; tries++ {
		var t track
		if len(again) > 0 {
			t, again = again[0], again[1:]
		} else {
			s.mu.Lock()
			g := s.games[chat]
			if g == nil || g.ID != id || len(g.Pool) == 0 {
				s.mu.Unlock()
				break
			}
			t, g.Pool = g.Pool[0], slices.Clone(g.Pool[1:])
			s.mu.Unlock()
		}
		m, err := s.fetch(s.ctx, t, length)
		if s.ctx.Err() != nil { // stopping: leave the game as it is, to be fetched after the restart
			s.mu.Lock()
			delete(s.preparing, id)
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.log.Printf("a track skipped: %v", err)
			continue
		}
		s.mu.Lock()
		delete(s.preparing, id)
		g := s.games[chat]
		if g == nil || g.ID != id {
			s.mu.Unlock()
			return
		}
		if k < len(g.Songs) {
			g.Songs[k] = t
		} else {
			g.Songs = append(g.Songs, t)
		}
		if s.media[id] == nil {
			s.media[id] = map[int]media{}
		}
		s.media[id][k] = m
		s.tryPost(g)
		s.mu.Unlock()
		s.save()
		return
	}
	s.mu.Lock()
	delete(s.preparing, id)
	g := s.games[chat]
	if g == nil || g.ID != id {
		s.mu.Unlock()
		return
	}
	s.log.Printf("game %s cut short: song %d of %d couldn't be fetched", id, k+1, g.Count)
	g.Count = k
	g.Songs = g.Songs[:min(len(g.Songs), k)]
	if g.Current == k && g.Post == "" { // it was due: the game is over
		s.finishLocked(g, nil)
	}
	s.mu.Unlock()
	s.save()
}

// prunePlayed forgets tracks played over playedDays ago, and the oldest
// past maxPlayed. s.mu is held.
func (s *server) prunePlayed(now time.Time) {
	since := now.Add(-playedDays * 24 * time.Hour)
	for chat, list := range s.played {
		list = slices.DeleteFunc(list, func(st stamp) bool { return st.At.Before(since) })
		if len(list) > maxPlayed {
			list = list[len(list)-maxPlayed:]
		}
		if len(list) == 0 {
			delete(s.played, chat)
		} else {
			s.played[chat] = list
		}
	}
}

// english is tracks, in order, without those not sung mainly in English:
// the bot's model decides, as Deezer doesn't say a track's language; without
// it, or if it fails, only tracks whose title or artist is in a script other
// than Latin are dropped. With SONGS_ANY_LANGUAGE, tracks as they are.
func (s *server) english(ctx context.Context, chat string, tracks []track, games bool) []track {
	if s.cfg.Anyway && !games || len(tracks) == 0 {
		return tracks
	}
	latin := slices.DeleteFunc(slices.Clone(tracks), func(t track) bool { return !isLatin(t.Title) || !isLatin(t.Artist) })
	if !s.srv.CanSample() || len(latin) == 0 {
		return latin
	}
	type item struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Artist string `json:"artist"`
	}
	var items []item
	for i, t := range latin {
		items = append(items, item{i + 1, t.Title, t.Artist})
	}
	body, _ := json.Marshal(items)
	zero := 0.0
	ctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()
	prompt := `You know popular music. For each track below, decide whether its lyrics are sung mainly in English. An instrumental, or a song mainly in another language, is not. Reply with JSON only, no other text: {"english":[1,3]}, the ids of the English ones.`
	if games {
		prompt = `You know video game music. For each track below, decide whether it is music from a video game: a game's soundtrack, theme or score, or a song made for a game. Music from a film or TV show, or a song merely used in a trailer, is not. Instrumentals are fine; a song with lyrics must be sung mainly in English. Reply with JSON only, no other text: {"english":[1,3]}, the ids of the tracks that qualify.`
	}
	res, err := s.srv.Sample(ctx, chat, addon.SampleRequest{
		SystemPrompt: prompt,
		MaxTokens:    judgeTokens, Temperature: &zero,
		Messages: []addon.SampleMessage{{Role: "user", Content: addon.Content{Type: "text", Text: string(body)}}},
	})
	if err != nil {
		s.log.Printf("choosing English songs failed, by script instead: %v", errClass(err))
		return latin
	}
	text := res.Content.Text
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	var r struct {
		English []int `json:"english"`
	}
	if i < 0 || j < i || json.Unmarshal([]byte(text[i:j+1]), &r) != nil {
		s.log.Printf("choosing English songs failed, by script instead: no usable reply")
		return latin
	}
	var out []track
	for k, t := range latin {
		if slices.Contains(r.English, k+1) {
			out = append(out, t)
		}
	}
	s.log.Printf("songs in English: %d of %d", len(out), len(latin))
	return out
}

// isLatin reports whether every letter of s is in the Latin script.
func isLatin(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return false
		}
	}
	return true
}

// source is where a game's songs come from: a decade's hits if decade
// isn't "", else genre's chart (0 for all genres; gamesGenre is the
// editors' game playlists).
type source struct {
	genre  int
	name   string // the genre's label
	decade string // the decade's label
}

// label is src as the intro names it: "" for all genres.
func (src source) label() string {
	switch {
	case src.decade != "":
		return src.decade
	case src.genre == 0:
		return ""
	}
	return src.name
}

// source is g's source of songs.
func (g *game) source() source {
	return source{genre: g.GenreID, name: g.Genre, decade: g.Decade}
}

// findSource is the source /song names: a decade, a genre of Deezer's
// list (fetched, unless it is kept), or all genres if name is "". It fails
// with the text to answer.
func (s *server) findSource(ctx context.Context, name string) (source, string) {
	if dec, ok := findDecade(name); ok {
		return source{decade: dec.Label}, ""
	}
	if name == "" {
		return source{}, ""
	}
	genres, err := s.deezer.Genres(ctx)
	if err != nil {
		return source{}, s.failText(ctx, err)
	}
	g, ok := findGenre(genres, name)
	if !ok {
		return source{}, unknownGenre(genres)
	}
	return source{genre: g.ID, name: genreLabel(g)}, ""
}

// failText logs why no game could start and is the text that says so:
// Deezer was too slow if ctx ended, else unreachable.
func (s *server) failText(ctx context.Context, err error) string {
	s.log.Printf("no game: %v", err)
	if ctx.Err() != nil {
		return tooSlow
	}
	return unreachable
}

// gather chooses a game's songs: src's tracks not played in chat lately,
// in English, up to n+poolExtra of them, and makes the first ready. It
// returns the first, its media and the tracks left, or the text that says
// why there are none.
func (s *server) gather(ctx context.Context, chat string, src source, n int, length time.Duration) (track, media, []track, string) {
	var chart []track
	var err error
	switch {
	case src.decade != "":
		dec, ok := findDecade(src.decade)
		if !ok {
			return track{}, media{}, nil, noSongs
		}
		chart, err = s.deezer.Decade(ctx, dec)
	case src.genre == gamesGenre: // the chart is mostly film songs: the editors' game playlists instead
		chart, err = s.deezer.Games(ctx)
	default:
		chart, err = s.deezer.Chart(ctx, src.genre)
	}
	if err != nil {
		return track{}, media{}, nil, s.failText(ctx, err)
	}
	pool := s.pick(chat, chart, 3*n+poolExtra) // more than needed: some may not be in English
	pool = s.english(ctx, chat, pool, src.genre == gamesGenre)
	pool = pool[:min(len(pool), n+poolExtra)]
	if len(pool) == 0 {
		if len(chart) == 0 {
			return track{}, media{}, nil, noSongs
		}
		return track{}, media{}, nil, allPlayed
	}
	t, m, pool, err := s.first(ctx, pool, length)
	if err != nil {
		s.log.Printf("no game: %v", err)
		if errors.Is(err, errNoSong) {
			return track{}, media{}, nil, noSongs
		}
		return track{}, media{}, nil, tooSlow
	}
	return t, m, pool, ""
}

// gatherRun is one choosing of a game's songs, told apart from a later one.
type gatherRun struct{ cancel context.CancelFunc }

// gatherGame starts choosing g's songs in the background, for at most
// prepMax, or until g ends or the server stops. s.mu is held.
func (s *server) gatherGame(g *game) {
	ctx, cancel := context.WithTimeout(s.ctx, prepMax)
	s.stopGathering(g.ID)
	run := &gatherRun{cancel}
	s.gathering[g.ID] = run
	chat, id, src, n, length := g.Chat, g.ID, g.source(), g.Asked, g.Clip
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer cancel()
		s.gathered(ctx, run, chat, id, src, n, length)
	}()
}

// stopGathering stops choosing the songs of the game id, if it is. s.mu
// is held.
func (s *server) stopGathering(id string) {
	if run := s.gathering[id]; run != nil {
		run.cancel()
		delete(s.gathering, id)
	}
}

// gathered chooses the songs of the game id in chat, once the bot has
// said what it offers, and gives them to the game, starting it if its
// lobby is over; if there are none, it calls the game off, saying why. A
// game that ended meanwhile is left alone, and one the server stopped
// during is chosen again after the restart.
func (s *server) gathered(ctx context.Context, run *gatherRun, chat, id string, src source, n int, length time.Duration) {
	var t track
	var m media
	var pool []track
	fail := noSongs // the bot never came back in time
	if s.waitForBot(ctx) {
		t, m, pool, fail = s.gather(ctx, chat, src, n, length)
	}
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	g := s.games[chat]
	if g == nil || g.ID != id || !g.Preparing || s.gathering[id] != run { // ended, or chosen again after a toggle
		s.mu.Unlock()
		return
	}
	delete(s.gathering, id)
	if fail != "" {
		s.log.Printf("game %s: no songs to play", id)
		s.callOffLocked(g, fail)
		s.mu.Unlock()
		s.save()
		return
	}
	g.Preparing = false
	g.Songs, g.Pool, g.Count = []track{t}, pool, min(n, 1+len(pool))
	g.Gathered, g.GatheredClip = n, length
	s.media[id] = map[int]media{0: m}
	s.log.Printf("game %s ready: %d songs", id, g.Count)
	if !g.Lobby { // its players were waiting for the songs
		g.Opened = s.clock.Now()
		s.tryPost(g)
	}
	s.mu.Unlock()
	s.save()
}

// waitForBot waits until the bot has said, in its handshake, what it
// offers, as it may not have yet just after a restart; false if ctx ends
// first.
func (s *server) waitForBot(ctx context.Context) bool {
	for !s.srv.WatchReplies() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return true
}
