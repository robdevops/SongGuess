package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/chat-plugin-songs/internal/state"
	"github.com/robdevops/signalbot/addon"
)

// server is the game: the chats' games, their scores and the songs played
// lately. srv speaks to the bot, and holds the outbox it reads.
type server struct {
	cfg    config
	clock  clock
	log    *log.Logger
	doc    *state.Doc
	srv    *addon.Server
	deezer *deezer

	// ctx ends when the server stops; the songs being prepared in the
	// background (bg) stop with it.
	ctx    context.Context
	cancel context.CancelFunc
	bg     sync.WaitGroup

	randMu sync.Mutex
	rand   *mrand.Rand

	mu        sync.Mutex
	games     map[string]*game         // by chat
	timers    map[string]stopper       // each game's reveal, by chat
	asks      map[string]stopper       // each game's next song, posted after the gap, by chat
	gap       time.Duration            // between a song's reveal and the next song: gapTime, or 0 in most tests
	starting  map[string]bool          // chats whose game is being set up
	media     map[string]map[int]media // the clips and covers made ready, by game, then song
	preparing map[string]bool          // games whose next song is being made ready
	scores    map[string][]score       // by chat, the last scoreDays days
	played    map[string][]stamp       // by chat, the tracks of the last playedDays days

	saveMu sync.Mutex // one save at a time, so the last one written is the newest
}

// media is a song made ready to play: its clip, and its cover for the
// reveal, if it could be fetched. Neither is saved: after a restart, a song
// not yet posted is fetched again, and a reveal goes without its cover.
type media struct {
	clip, cover []byte
}

// game is one chat's game. Its answers are kept only as right or wrong:
// what anyone replied is never kept.
type game struct {
	ID       string            `json:"id"` // also the watch's
	Chat     string            `json:"chat"`
	Starter  string            `json:"starter"` // who may end it
	Genre    string            `json:"genre,omitempty"`
	Count    int               `json:"count"`           // the songs it will play; fewer than asked if the chart runs out
	Asked    int               `json:"asked,omitempty"` // the songs /song asked for
	Clip     time.Duration     `json:"clip"`            // each clip's length
	Time     time.Duration     `json:"time"`            // to answer each song
	Started  time.Time         `json:"started"`
	End      time.Time         `json:"end"`     // when the last song is due to be revealed, with room to fetch songs
	Songs    []track           `json:"songs"`   // made ready or played, in order
	Pool     []track           `json:"pool"`    // the chart's tracks still to try
	Current  int               `json:"current"` // the open song, from 0
	Post     string            `json:"post"`    // the outbox op that posted it, for the reveal to reply to; "" until then
	Opened   time.Time         `json:"opened"`  // when it was posted, or is due to be
	Deadline time.Time         `json:"deadline"`
	Answers  map[string]answer `json:"answers"` // the open song's, by player
	Players  []string          `json:"players"` // who answered song 1: a later song ends once they all have
	Seq      int               `json:"seq"`     // the answers recorded so far, to break ties
	Points   map[string]int    `json:"points"`  // by player
	Right    map[string]int    `json:"right"`   // songs with something right, by player
}

// answer is what one player's last answer to the open song got right, and
// when it was sent.
type answer struct {
	Title  bool      `json:"title"`
	Artist bool      `json:"artist"`
	At     time.Time `json:"at"`
	Seq    int       `json:"seq"`
}

// score is a player's points from one game.
type score struct {
	User   string    `json:"user"`
	Points int       `json:"points"`
	At     time.Time `json:"at"`
}

// stamp is a track played in a chat: Deezer's ID for it, and when.
type stamp struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
}

// saved is songs.json.
type saved struct {
	Games  []*game            `json:"games"`
	Scores map[string][]score `json:"scores"`
	Played map[string][]stamp `json:"played"`
	Outbox []addon.Held       `json:"outbox"`
}

// The state's limits, and the slack on a song's end.
const (
	scoreDays  = 7
	playedDays = 30
	maxPlayed  = 3000 // tracks remembered per chat
	// settle is how long after its deadline a song is revealed, so an
	// answer sent in time that waited in the bot's queue still counts.
	settle = 3 * time.Second
	// watchSlack is how long after the game's end the bot keeps the watch,
	// so a late answer still stays out of the chat's history.
	watchSlack = 10 * time.Second
	// lateSlack is how late a song may be posted before it no longer fits
	// within the watch.
	lateSlack = 5 * time.Second
	// fetchSlack is the time a game allows, in all, for songs that aren't
	// ready when they are due.
	fetchSlack = 30 * time.Second
	// gapTime is the pause between a song's reveal and the next song, so
	// players can read it.
	gapTime = 5 * time.Second
)

// newServer loads the state saved in cfg.StateDir, re-arms its games'
// timers, reveals at once the songs whose time passed while it was down,
// and fetches again the songs that weren't posted yet.
func newServer(cfg config, clk clock, rnd *mrand.Rand, client *http.Client, logger *log.Logger) (*server, error) {
	s := &server{
		cfg: cfg, clock: clk, log: logger, rand: rnd,
		doc:       state.Open(cfg.StateDir, "songs"),
		deezer:    newDeezer(cfg.Deezer, client),
		games:     map[string]*game{},
		timers:    map[string]stopper{},
		asks:      map[string]stopper{},
		gap:       cfg.gap,
		starting:  map[string]bool{},
		media:     map[string]map[int]media{},
		preparing: map[string]bool{},
		scores:    map[string][]score{},
		played:    map[string][]stamp{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.srv = addon.New(addon.Config{Name: "songs", Title: "Songs", Version: "1", Tools: s.tools(), Now: clk.Now, Changed: s.save})
	var st saved
	if _, err := s.doc.Load(&st); err != nil {
		return nil, err
	}
	if st.Scores != nil {
		s.scores = st.Scores
	}
	if st.Played != nil {
		s.played = st.Played
	}
	s.srv.Restore(st.Outbox)
	s.mu.Lock()
	revealed := 0
	for _, g := range st.Games {
		if g == nil || s.games[g.Chat] != nil || g.Current >= g.Count || g.Current > len(g.Songs) {
			continue
		}
		if g.Answers == nil {
			g.Answers = map[string]answer{}
		}
		if g.Points == nil {
			g.Points = map[string]int{}
		}
		if g.Right == nil {
			g.Right = map[string]int{}
		}
		s.games[g.Chat] = g
		switch {
		case g.Post == "": // its song wasn't posted yet: fetch it again, and post it when due
			s.scheduleAsk(g)
		case !s.clock.Now().Before(g.Deadline.Add(settle)):
			s.revealLocked(g)
			revealed++
		default:
			s.arm(g)
			s.prepareNext(g)
		}
	}
	open := len(s.games)
	s.mu.Unlock()
	s.log.Printf("started: %d games open, %d songs revealed late, %d ops held", open, revealed, len(s.srv.Outbox()))
	if revealed > 0 {
		s.save()
	}
	return s, nil
}

// shutdown stops the songs being fetched, and waits for them.
func (s *server) shutdown() {
	s.cancel()
	s.bg.Wait()
}

// save writes songs.json, logging a failure; the state stays in memory,
// and the next change tries again.
func (s *server) save() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := saved{Scores: s.scores, Played: s.played, Outbox: s.srv.Outbox(), Games: make([]*game, 0, len(s.games))}
	for _, g := range s.games {
		st.Games = append(st.Games, g)
	}
	slices.SortFunc(st.Games, func(a, b *game) int { return strings.Compare(a.Chat, b.Chat) })
	data, err := json.Marshal(st)
	s.mu.Unlock()
	if err == nil {
		err = s.doc.Save(json.RawMessage(data))
	}
	if err != nil {
		s.log.Printf("saving failed: %v", err)
	}
}

// randomID is n random bytes in hex.
func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// validID reports whether id is one of signalbot's pseudonyms with prefix
// (c_ or u_), so it can go into a {u_…} placeholder safely.
func validID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || rest == "" || len(rest) > 100 {
		return false
	}
	for _, c := range rest {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// queue adds ops to the outbox and returns the IDs they got. A clip and a
// cover are well under the outbox's limits, so it never refuses them; if
// it did, the log says so.
func (s *server) queue(ops ...addon.Op) []string {
	ids, err := s.srv.QueueIDs(ops...)
	if err != nil {
		s.log.Printf("an op was refused: %v", err)
		return make([]string, len(ops))
	}
	return ids
}
