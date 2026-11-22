package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/addon"
)

// The texts players see.
const (
	noChat      = "The song quiz works only in a chat."
	noGame      = "There's no song quiz on. Start one with /song."
	needsWatch  = "The song quiz needs a signalbot that takes reply watches: update the bot."
	settingUp   = "A song quiz is being set up for this chat; give it a moment."
	unreachable = "I couldn't reach Deezer just now. Try again in a minute."
	tooSlow     = "Deezer was too slow just now. Try again."
	noSongs     = "I couldn't get a song to play just now. Try again, or try another genre."
	allPlayed   = "Every song in that chart was played here in the last 30 days. Try another genre."
	badCount    = "A song quiz can have 1 to 10 songs, such as /song rock 5."
	badClip     = "A clip can be 1s to 25s long, such as /song rock 10s."
	notYet      = "Wait for the next song."
	tooLate     = "Too late for that song."
	credit      = "Previews from Deezer"
)

// usage is the help for /song.
const usage = "Try /song, /song rock, /song pop 10s 3, /song genres, /song end, /song scores, or /song help."

// Points for the title and the artist, and the bonus for the first player
// with either right.
const (
	titlePoints  = 10
	artistPoints = 5
	firstBonus   = 5
)

// The bot's limits: what it leaves a direct command (when the call carries
// no deadline), what the game leaves of it to start and reply, and the
// longest post it sends.
const (
	defaultWait = 55 * time.Second
	replyTime   = 3 * time.Second
	maxPost     = 4000 // runes, once the bot puts names in
	maxNamed    = 20   // players named under one song's title or artist
	maxRanked   = 10   // players in the results and scores
	maxGenres   = 15   // genres named when one isn't known
)

// tools are the game's tools, both for the bot alone: neither is offered
// to the model.
func (s *server) tools() []addon.Tool {
	return []addon.Tool{
		{
			Name:        "song",
			Description: "The /song command: start a song quiz in this chat, end it, or show its scores.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"args":{"type":"string"}}}`),
			Visibility:  []string{"bot"},
			Call: func(ctx context.Context, c *addon.Context, raw json.RawMessage) (addon.Result, error) {
				var a struct {
					Args string `json:"args"`
				}
				if err := addon.DecodeArgs(raw, &a); err != nil {
					return addon.Result{}, err
				}
				return s.command(ctx, c, a.Args), nil
			},
		},
		{
			Name:        "song_answer",
			Description: "Records one reply to the song quiz's messages as an answer to the open song.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
			Visibility:  []string{"bot"},
			Call: func(_ context.Context, c *addon.Context, raw json.RawMessage) (addon.Result, error) {
				var a struct {
					Text string `json:"text"`
				}
				if err := addon.DecodeArgs(raw, &a); err != nil {
					return addon.Result{}, err
				}
				return s.answer(c, a.Text), nil
			},
		},
	}
}

// fromChat reports whether c names a chat and a sender.
func fromChat(c *addon.Context) bool {
	return c != nil && validID(c.Chat, "c_") && validID(c.Sender, "u_")
}

// command is /song with its arguments: a genre, a clip length and a
// number of songs, the last two at the end in either order.
func (s *server) command(ctx context.Context, c *addon.Context, args string) addon.Result {
	if !fromChat(c) {
		return addon.Result{Content: addon.Text(noChat).Content, IsError: true}
	}
	fields := strings.Fields(args)
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "end":
			return s.finish(c)
		case "scores":
			return addon.Text(s.scoreboard(c.Chat))
		case "help":
			return addon.Text(s.helpText())
		case "genre", "genres":
			return addon.Text(s.genreList(ctx))
		}
	}
	n, length := s.cfg.Songs, clipLength
	gotN, gotClip := false, false
	for len(fields) > 0 {
		last := fields[len(fields)-1]
		if !gotN && allDigits(last) {
			v, err := songCount(last)
			if err != nil {
				return addon.Text(badCount)
			}
			n, gotN = v, true
			fields = fields[:len(fields)-1]
			continue
		}
		if _, isDecade := findDecade(last); isDecade { // /song 80s: a decade, not 80 seconds; 10s is a clip length
			if _, err := clipArg(last); err != nil {
				break
			}
		}
		if !gotClip && startsWithDigit(last) {
			if _, err := time.ParseDuration(strings.ToLower(last)); err == nil {
				d, err := clipArg(last)
				if err != nil {
					return addon.Text(badClip)
				}
				length, gotClip = d, true
				fields = fields[:len(fields)-1]
				continue
			}
		}
		break
	}
	return s.start(ctx, c, strings.Join(fields, " "), n, length)
}

func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// busy is the answer to /song while a game is on.
func busy(g *game) addon.Result {
	return addon.Text(fmt.Sprintf("A song quiz is on: song %d of %d.", g.Current+1, g.Count))
}

// start finds the chart of genreName (all genres if ""), makes its first
// song ready and begins a game of n songs, answering with the intro and
// the watch that takes the replies. The first song is a post of its own,
// queued now, so it follows the intro; the rest are fetched in the
// background, each before it is due.
func (s *server) start(ctx context.Context, c *addon.Context, genreName string, n int, length time.Duration) addon.Result {
	s.mu.Lock()
	if g := s.games[c.Chat]; g != nil {
		s.mu.Unlock()
		return busy(g)
	}
	if !s.srv.WatchReplies() {
		s.mu.Unlock()
		return addon.Text(needsWatch)
	}
	if s.starting[c.Chat] {
		s.mu.Unlock()
		return addon.Text(settingUp)
	}
	s.starting[c.Chat] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.starting, c.Chat)
		s.mu.Unlock()
	}()

	// Leave the bot time to send the intro.
	deadline := time.Now().Add(defaultWait)
	if c.Deadline != 0 {
		deadline = time.Unix(c.Deadline, 0).Add(-replyTime)
	}
	gctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	failed := func(err error) addon.Result {
		s.log.Printf("no game: %v", err)
		if gctx.Err() != nil {
			return addon.Text(tooSlow)
		}
		return addon.Text(unreachable)
	}
	chosen := genre{ID: 0}
	var chart []track
	var err error
	if dec, ok := findDecade(genreName); ok {
		chosen = genre{ID: -1, Name: dec.Label}
		if chart, err = s.deezer.Decade(gctx, dec); err != nil {
			return failed(err)
		}
	} else if genreName != "" {
		genres, err := s.deezer.Genres(gctx)
		if err != nil {
			return failed(err)
		}
		g, ok := findGenre(genres, genreName)
		if !ok {
			return addon.Text(unknownGenre(genres))
		}
		chosen = g
	}
	if chosen.ID >= 0 {
		if chart, err = s.deezer.Chart(gctx, chosen.ID); err != nil {
			return failed(err)
		}
	}
	pool := s.pick(c.Chat, chart, n)
	if len(pool) == 0 {
		if len(chart) == 0 {
			return addon.Text(noSongs)
		}
		return addon.Text(allPlayed)
	}
	t, m, pool, err := s.first(gctx, pool, length)
	if err != nil {
		s.log.Printf("no game: %v", err)
		if errors.Is(err, errNoSong) {
			return addon.Text(noSongs)
		}
		return addon.Text(tooSlow)
	}
	if ctx.Err() != nil { // the bot gave up: it won't show the intro
		s.log.Printf("game dropped: the bot stopped waiting")
		return addon.Text(tooSlow)
	}

	s.mu.Lock()
	if g := s.games[c.Chat]; g != nil {
		s.mu.Unlock()
		return busy(g)
	}
	now := s.clock.Now()
	count := min(n, 1+len(pool))
	g := &game{
		ID: "s" + randomID(6), Chat: c.Chat, Starter: c.Sender, Genre: chosen.Name,
		Count: count, Asked: n, Clip: length, Time: s.cfg.Time,
		Started: now, End: now.Add(time.Duration(count)*(s.cfg.Time+settle) + time.Duration(count-1)*s.gap + fetchSlack),
		Songs: []track{t}, Pool: pool, Opened: now,
		Answers: map[string]answer{}, Points: map[string]int{}, Right: map[string]int{},
	}
	if chosen.ID == 0 {
		g.Genre = ""
	}
	s.games[c.Chat] = g
	s.media[g.ID] = map[int]media{0: m}
	s.tryPost(g)
	text := intro(g)
	s.mu.Unlock()
	s.save()
	s.log.Printf("game %s started: %d songs, %s clips, %s to answer", g.ID, count, length, g.Time)
	return addon.Result{
		Content: addon.Text(clamp(text, maxPost)).Content,
		Meta: &addon.ResultMeta{Watch: &addon.Watch{
			ID: g.ID, Replies: true, Until: g.End.Add(watchSlack).Format(time.RFC3339), Tool: "song_answer",
		}},
	}
}

// helpText is /song help: every command, with the defaults in use.
func (s *server) helpText() string {
	return fmt.Sprintf("🎵 **Song quiz**: guess a song from a short clip, by replying with its title or artist.\n"+
		"/song — %d %s from the main chart, %s clips\n"+
		"/song rock — a genre's chart; names match loosely (rap, hiphop, rnb)\n"+
		"/song 80s — a decade's hits: 60s, 70s, 80s, 90s, 2000s, 2010s\n"+
		"/song rock 10s 3 — a clip length (1s to 25s) and a number of songs (1 to 10), at the end, in either order\n"+
		"/song genres — the genres and decades\n"+
		"/song end — finish now (only whoever started it)\n"+
		"/song scores — this chat's totals over the last 7 days\n"+
		"Each song: %s to answer. The title %d points, the artist %d, and %d more for the first right.",
		s.cfg.Songs, plural(s.cfg.Songs, "song", "songs"), lengthLabel(clipLength), lengthLabel(s.cfg.Time), titlePoints, artistPoints, firstBonus)
}

// unknownGenre is the answer to /song with a genre Deezer hasn't, naming
// some it has.
func unknownGenre(genres []genre) string {
	return "I don't know that genre. Try one of: " + genreNames(genres, maxGenres) + ", or /song genres for them all."
}

// genreNames are Deezer's genres, at most limit of them, as /song takes
// them.
func genreNames(genres []genre, limit int) string {
	var names []string
	for _, g := range genres {
		if g.ID != 0 && len(names) < limit {
			names = append(names, strings.ToLower(g.Name))
		}
	}
	return strings.Join(names, ", ")
}

// genreList is /song genres: every genre Deezer has.
func (s *server) genreList(ctx context.Context) string {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	genres, err := s.deezer.Genres(gctx)
	if err != nil || genreNames(genres, len(genres)) == "" {
		return unreachable
	}
	return "🎵 **Genres:** " + genreNames(genres, len(genres)) + ".\n**Decades:** " + decadeNames() + ".\nPlay one with, say, /song rock or /song 80s; plain /song plays the main chart."
}

// decadeNames are the decades /song plays, as it takes them.
func decadeNames() string {
	var names []string
	for _, d := range decades {
		names = append(names, d.Label)
	}
	return strings.Join(names, ", ")
}

// intro is the game's first lines: its genre, its length, and how to
// answer.
func intro(g *game) string {
	title := "🎵 **Song quiz**"
	if g.Genre != "" {
		title = "🎵 **Song quiz: " + g.Genre + "**"
	}
	short := ""
	if g.Asked > g.Count {
		short = fmt.Sprintf(" (that's all the chart has that hasn't been played here lately, of the %d asked for)", g.Asked)
	}
	return fmt.Sprintf("%s · %d %s%s, %s clips, %s to answer each.\n"+
		"**Reply** to each clip with the title or the artist, or both, such as \"Title by Artist\": "+
		"%d points for the title, %d for the artist, and %d more for the first to get either. "+
		"Your last answer before time is up counts. From song 2 on, a song ends as soon as everyone who answered song 1 has answered.",
		title, g.Count, plural(g.Count, "song", "songs"), short, lengthLabel(g.Clip), lengthLabel(g.Time),
		titlePoints, artistPoints, firstBonus)
}

// songText is the open song's post.
func songText(g *game) string {
	return fmt.Sprintf("🎵 **Song %d of %d** · reply to this message with the title or the artist", g.Current+1, g.Count)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lengthLabel is a length as players see it: 5s, 2 min, 1m30s.
func lengthLabel(d time.Duration) string {
	d = d.Round(time.Second)
	switch m, sec := int(d/time.Minute), int(d%time.Minute/time.Second); {
	case m == 0:
		return strconv.Itoa(sec) + "s"
	case sec == 0:
		return strconv.Itoa(m) + " min"
	default:
		return fmt.Sprintf("%dm%ds", m, sec)
	}
}

// tryPost posts g's open song if it is due and its clip is ready, giving
// it its time to answer, or what is left of the watch; if too little is
// left, the game ends. A song not ready is fetched, and posted once it is.
// s.mu is held.
func (s *server) tryPost(g *game) {
	now := s.clock.Now()
	if g.Post != "" || now.Before(g.Opened) {
		return
	}
	m, ok := s.media[g.ID][g.Current]
	if !ok || g.Current >= len(g.Songs) {
		s.prepareNext(g)
		return
	}
	length := min(g.Time, g.End.Add(lateSlack).Sub(now)-settle)
	if length < minTime {
		s.log.Printf("game %s cut short: %d songs left that no longer fit", g.ID, g.Count-g.Current)
		s.finishLocked(g, nil)
		return
	}
	g.Post = s.queue(addon.Op{
		Op: "post", Chat: g.Chat, Text: songText(g),
		Audio: &addon.Audio{Data: base64.StdEncoding.EncodeToString(m.clip), MimeType: "audio/mpeg"},
	})[0]
	g.Opened, g.Deadline = now, now.Add(length)
	s.media[g.ID][g.Current] = media{cover: m.cover} // the clip is in the outbox now
	s.prunePlayed(now)
	s.played[g.Chat] = append(s.played[g.Chat], stamp{ID: g.Songs[g.Current].ID, At: now})
	s.arm(g)
	s.prepareNext(g)
}

// arm sets g's timer to reveal its open song once its time is up. s.mu is
// held.
func (s *server) arm(g *game) {
	chat, id, current := g.Chat, g.ID, g.Current
	if t := s.timers[chat]; t != nil {
		t.Stop()
	}
	s.timers[chat] = s.clock.AfterFunc(g.Deadline.Add(settle).Sub(s.clock.Now()), func() { s.timeUp(chat, id, current) })
}

// timeUp reveals song current of the game id in chat, if it is still
// open.
func (s *server) timeUp(chat, id string, current int) {
	s.mu.Lock()
	g := s.games[chat]
	if g == nil || g.ID != id || g.Current != current || g.Post == "" {
		s.mu.Unlock()
		return
	}
	s.revealLocked(g)
	s.mu.Unlock()
	s.save()
}

// revealLocked marks g's open song and queues its reveal, as a reply to
// it; then the next song, after the gap, or after the last the results
// and the end of the watch. s.mu is held.
func (s *server) revealLocked(g *game) {
	now := s.clock.Now()
	if g.Current == 0 {
		g.Players = slices.Sorted(maps.Keys(g.Answers))
	}
	reveal := s.revealOp(g)
	delete(s.media[g.ID], g.Current)
	next := g.Current + 1
	if next >= g.Count {
		s.finishLocked(g, []addon.Op{reveal})
		return
	}
	s.queue(reveal)
	g.Current, g.Opened, g.Deadline, g.Answers, g.Post = next, now.Add(s.gap), time.Time{}, map[string]answer{}, ""
	s.scheduleAsk(g)
}

// scheduleAsk posts g's open song when it is due: at once if it is, else
// by a timer. s.mu is held.
func (s *server) scheduleAsk(g *game) {
	if t := s.asks[g.Chat]; t != nil {
		t.Stop()
		delete(s.asks, g.Chat)
	}
	wait := g.Opened.Sub(s.clock.Now())
	if wait <= 0 {
		s.tryPost(g)
		return
	}
	s.prepareNext(g) // in case it isn't on its way
	chat, id, current := g.Chat, g.ID, g.Current
	s.asks[chat] = s.clock.AfterFunc(wait, func() {
		s.mu.Lock()
		g := s.games[chat]
		if g == nil || g.ID != id || g.Current != current || g.Post != "" {
			s.mu.Unlock()
			return
		}
		delete(s.asks, chat)
		s.tryPost(g)
		s.mu.Unlock()
		s.save()
	})
}

// revealOp is the post revealing g's open song, as a reply to it, with its
// cover if it came. It scores the song. s.mu is held.
func (s *server) revealOp(g *game) addon.Op {
	op := addon.Op{Op: "post", Chat: g.Chat, Text: clamp(s.mark(g), maxPost), ReplyTo: g.Post}
	if m := s.media[g.ID][g.Current]; m.cover != nil {
		op.Images = []addon.Image{{Data: base64.StdEncoding.EncodeToString(m.cover), MimeType: "image/jpeg", Name: "cover.jpg"}}
	}
	return op
}

// mark scores g's open song and returns its reveal: the title and artist,
// who got each, as {u_…} placeholders, the first marked ⚡, and Deezer's
// credit. s.mu is held.
func (s *server) mark(g *game) string {
	t := g.Songs[g.Current]
	type right struct {
		user string
		answer
	}
	var rights []right
	for user, a := range g.Answers {
		if a.Title || a.Artist {
			rights = append(rights, right{user, a})
		}
	}
	slices.SortFunc(rights, func(a, b right) int { return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Seq, b.Seq)) })
	for i, r := range rights {
		p := 0
		if r.Title {
			p += titlePoints
		}
		if r.Artist {
			p += artistPoints
		}
		if i == 0 {
			p += firstBonus
		}
		g.Points[r.user] += p
		g.Right[r.user]++
	}
	var b strings.Builder
	b.WriteString("✅ **" + t.Title + "** — " + t.Artist)
	if len(rights) == 0 {
		b.WriteString("\nNobody got it.")
	}
	first := ""
	if len(rights) > 0 {
		first = rights[0].user
	}
	for _, line := range []struct {
		label string
		got   func(answer) bool
	}{{"Title", func(a answer) bool { return a.Title }}, {"Artist", func(a answer) bool { return a.Artist }}} {
		var named []string
		more := 0
		for _, r := range rights {
			if !line.got(r.answer) {
				continue
			}
			if len(named) == maxNamed {
				more++
				continue
			}
			name := "{" + r.user + "}"
			if r.user == first {
				name += " ⚡"
				first = "" // marked once
			}
			named = append(named, name)
		}
		if len(named) == 0 {
			continue
		}
		b.WriteString("\n" + line.label + ": " + strings.Join(named, ", "))
		if more > 0 {
			fmt.Fprintf(&b, " and %d more", more)
		}
	}
	b.WriteString("\n" + credit)
	return b.String()
}

// finish is /song end, which only the player who started the game may
// use. It answers with the open song's reveal and the results, and queues
// the end of the game's watch.
func (s *server) finish(c *addon.Context) addon.Result {
	s.mu.Lock()
	g := s.games[c.Chat]
	switch {
	case g == nil:
		s.mu.Unlock()
		return addon.Text(noGame)
	case g.Starter != c.Sender:
		s.mu.Unlock()
		return addon.Text("Only the player who started this song quiz can end it.")
	}
	revealed := ""
	if g.Post != "" { // the open song was posted: reveal it first
		revealed = s.mark(g) + "\n\n"
	}
	text := revealed + s.end(g, s.clock.Now())
	s.queue(addon.Op{Op: "unwatch", Chat: g.Chat, Watch: g.ID})
	s.mu.Unlock()
	s.save()
	return addon.Text(clamp(text, maxPost))
}

// finishLocked ends g, queueing ops (its last reveal), the results and
// the end of the watch. s.mu is held.
func (s *server) finishLocked(g *game, ops []addon.Op) {
	results := s.end(g, s.clock.Now())
	s.queue(append(ops, addon.Op{Op: "post", Chat: g.Chat, Text: clamp(results, maxPost)}, addon.Op{Op: "unwatch", Chat: g.Chat, Watch: g.ID})...)
}

// end closes g, adding its points to the chat's scores, and returns its
// results. s.mu is held.
func (s *server) end(g *game, now time.Time) string {
	delete(s.games, g.Chat)
	delete(s.media, g.ID)
	for _, m := range []map[string]stopper{s.timers, s.asks} {
		if t := m[g.Chat]; t != nil {
			t.Stop()
			delete(m, g.Chat)
		}
	}
	type placing struct {
		user          string
		points, right int
	}
	var placings []placing
	for user, p := range g.Points {
		placings = append(placings, placing{user, p, g.Right[user]})
	}
	slices.SortFunc(placings, func(a, b placing) int {
		return cmp.Or(cmp.Compare(b.points, a.points), strings.Compare(a.user, b.user))
	})
	s.pruneScores(now)
	for _, p := range placings {
		s.scores[g.Chat] = append(s.scores[g.Chat], score{User: p.user, Points: p.points, At: now})
	}
	played := g.Current
	if g.Post != "" {
		played++
	}
	s.log.Printf("game %s ended after %d of %d songs: %d players scored", g.ID, played, g.Count, len(placings))
	if len(placings) == 0 {
		return "🏁 **Song quiz over.** Nobody scored."
	}
	var b strings.Builder
	b.WriteString("🏁 **Song quiz over**")
	for i, p := range placings[:min(len(placings), maxRanked)] {
		fmt.Fprintf(&b, "\n%s {%s} %d · %d/%d songs", medal(i+1), p.user, p.points, p.right, played)
	}
	return b.String()
}

// medal marks a rank: a medal for the first three, else the number.
func medal(rank int) string {
	if rank <= 3 {
		return []string{"🥇", "🥈", "🥉"}[rank-1]
	}
	return strconv.Itoa(rank) + "."
}

// answer records one reply to the game's messages as its sender's answer
// to the open song: only whether it named the title and the artist, never
// what it said. A player's last answer before the deadline counts, by when
// the bot got it: a new reply or an edit replaces an earlier answer.
// A recorded answer gets no reaction: reactions take from the chat's post
// budget, and a reveal post refused by it is lost.
func (s *server) answer(c *addon.Context, msg string) addon.Result {
	if !fromChat(c) {
		return addon.Text(noChat)
	}
	at := s.clock.Now()
	if t := cmp.Or(c.Edited, c.Sent); t > 0 {
		at = time.Unix(t, 0)
	}
	s.mu.Lock()
	g := s.games[c.Chat]
	switch {
	case g == nil:
		s.mu.Unlock()
		return addon.Text(noGame)
	case c.Sent > 0 && c.Sent < g.Started.Unix():
		s.mu.Unlock()
		return addon.Text("That message is from before this song quiz.")
	case g.Post == "":
		s.mu.Unlock()
		return addon.Text(notYet)
	case c.Sent > 0 && time.Unix(c.Sent, 0).Before(g.Opened): // an edit of an answer to an earlier song
		s.mu.Unlock()
		return addon.Text(tooLate)
	case at.Before(g.Opened) || at.After(g.Deadline): // at is in whole seconds
		s.mu.Unlock()
		return addon.Text(tooLate)
	}
	if prev, ok := g.Answers[c.Sender]; ok && at.Before(prev.At) { // an edit of an older answer, arriving late
		s.mu.Unlock()
		return addon.Text("A later answer of yours counts.")
	}
	t := g.Songs[g.Current]
	g.Seq++
	g.Answers[c.Sender] = answer{Title: matches(msg, titleNames(t)), Artist: matches(msg, artistNames(t)), At: at, Seq: g.Seq}
	if s.allAnswered(g) {
		// Everyone playing has answered: end the song now, revealing it
		// after settle, as at its deadline, for answers still on their way.
		g.Deadline = s.clock.Now()
		s.arm(g)
		s.log.Printf("game %s song %d ended early", g.ID, g.Current+1)
	}
	s.mu.Unlock()
	s.save()
	return addon.Text("Recorded.")
}

// allAnswered reports whether a song after the first has an answer from
// each player of song 1, and is still before its deadline. s.mu is held.
func (s *server) allAnswered(g *game) bool {
	if g.Current == 0 || len(g.Players) == 0 || !s.clock.Now().Before(g.Deadline) {
		return false
	}
	for _, p := range g.Players {
		if _, ok := g.Answers[p]; !ok {
			return false
		}
	}
	return true
}

// scoreboard is /song scores: the chat's totals over the last scoreDays.
func (s *server) scoreboard(chat string) string {
	s.mu.Lock()
	s.pruneScores(s.clock.Now())
	type total struct {
		user          string
		points, games int
	}
	byUser := map[string]*total{}
	var totals []*total
	for _, sc := range s.scores[chat] {
		t := byUser[sc.User]
		if t == nil {
			t = &total{user: sc.User}
			byUser[sc.User] = t
			totals = append(totals, t)
		}
		t.points += sc.Points
		t.games++
	}
	s.mu.Unlock()
	if len(totals) == 0 {
		return "No song quiz scores in the last 7 days. Start one with /song."
	}
	// Scores are kept in the order they were won, so a tie goes to who
	// scored first.
	slices.SortStableFunc(totals, func(a, b *total) int { return cmp.Compare(b.points, a.points) })
	var b strings.Builder
	b.WriteString("🏆 **Song quiz scores, last 7 days**")
	for i, t := range totals[:min(len(totals), maxRanked)] {
		fmt.Fprintf(&b, "\n%s {%s} %d · %d %s", medal(i+1), t.user, t.points, t.games, plural(t.games, "game", "games"))
	}
	return b.String()
}

// pruneScores forgets scores older than scoreDays. s.mu is held.
func (s *server) pruneScores(now time.Time) {
	since := now.Add(-scoreDays * 24 * time.Hour)
	for chat, list := range s.scores {
		s.scores[chat] = slices.DeleteFunc(list, func(sc score) bool { return sc.At.Before(since) })
		if len(s.scores[chat]) == 0 {
			delete(s.scores, chat)
		}
	}
}
