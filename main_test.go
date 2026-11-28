package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/addon"
)

// fakeClock is a clock whose time moves only when a test says.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the time on by d, firing the timers then due in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool {
		if !t.at.After(c.now) {
			due = append(due, t)
			return true
		}
		return false
	})
	c.mu.Unlock()
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
	for _, t := range due {
		if !t.stopped {
			t.f()
		}
	}
}

// syncBuffer is a log a test can read while the server may write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The songs the fake Deezer serves.
var songs = []track{
	{ID: 101, Title: "Bohemian Rhapsody (Remastered 2011)", Short: "Bohemian Rhapsody", Artist: "Queen"},
	{ID: 102, Title: "Dancing Queen", Artist: "ABBA"},
	{ID: 103, Title: "Blinding Lights", Artist: "The Weeknd"},
	{ID: 104, Title: "Uptown Funk (feat. Bruno Mars)", Short: "Uptown Funk", Artist: "Mark Ronson"},
	{ID: 105, Title: "Hey Jude - Remastered 2015", Short: "Hey Jude - Remastered 2015", Artist: "The Beatles"},
	{ID: 106, Title: "Rolling in the Deep", Artist: "Adele"},
	{ID: 107, Title: "Smells Like Teen Spirit", Artist: "Nirvana"},
	{ID: 108, Title: "Billie Jean", Artist: "Michael Jackson"},
}

// rockSongs are rock's chart: one song that plays, with no cover, and two
// whose previews fail.
var rockSongs = []track{
	{ID: 201, Title: "Back in Black", Artist: "AC/DC"},
	{ID: 202, Title: "Paranoid", Artist: "Black Sabbath"},
	{ID: 203, Title: "Highway Star", Artist: "Deep Purple"},
}

// fakeDeezer is Deezer's API, previews and covers, as the tests serve it.
type fakeDeezer struct {
	srv *httptest.Server

	mu       sync.Mutex
	charts   map[string][]track // by genre ID
	failing  map[int64]bool     // tracks whose preview fails
	noCover  map[int64]bool
	requests []string
	hold     chan struct{} // if set, charts and playlists wait for it to close
	dropped  int           // the held requests given up on by the game
}

// block holds charts and playlists until release.
func (d *fakeDeezer) block() {
	d.mu.Lock()
	d.hold = make(chan struct{})
	d.mu.Unlock()
}

// release lets the held charts and playlists go.
func (d *fakeDeezer) release() {
	d.mu.Lock()
	close(d.hold)
	d.hold = nil
	d.mu.Unlock()
}

// gaveUp is the number of held requests the game gave up on.
func (d *fakeDeezer) gaveUp() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

func newFakeDeezer(t *testing.T) *fakeDeezer {
	d := &fakeDeezer{
		charts:  map[string][]track{"0": songs, "152": rockSongs, "116": songs[:2], "pl867825522": rockSongs, "pl868": songs[:3], "pl14917741483": rockSongs, "pl4380597982": rockSongs, "pl13650084141": rockSongs},
		failing: map[int64]bool{202: true, 203: true},
		noCover: map[int64]bool{201: true},
	}
	audio := mp3(1150)
	var cover bytes.Buffer
	jpeg.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.requests = append(d.requests, r.URL.RequestURI())
		d.mu.Unlock()
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		path := r.URL.Path
		switch {
		case path == "/genre":
			io.WriteString(w, `{"data":[{"id":0,"name":"All"},{"id":132,"name":"Pop"},{"id":152,"name":"Rock"},{"id":116,"name":"Rap/Hip Hop"},{"id":165,"name":"R&B"},{"id":173,"name":"Films/Games"}]}`)
		case path == "/search/playlist":
			// A fan's list first, then the editors' "80s Hits", whose ID the
			// test charts hold as "pl" + ID.
			io.WriteString(w, `{"data":[{"id":5,"title":"80s Hits","user":{"name":"Alice"}},{"id":867,"title":"80s Party Hits","user":{"name":"Laeti - Deezer Pop Editor"}},{"id":868,"title":"80s Hits","user":{"name":"Laeti - Deezer Pop Editor"}}]}`)
		case strings.HasPrefix(path, "/chart/") && strings.HasSuffix(path, "/tracks"),
			strings.HasPrefix(path, "/playlist/") && strings.HasSuffix(path, "/tracks"):
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("chart %s", r.URL)
			}
			d.mu.Lock()
			hold := d.hold
			d.mu.Unlock()
			if hold != nil {
				select {
				case <-hold:
				case <-r.Context().Done():
					d.mu.Lock()
					d.dropped++
					d.mu.Unlock()
					return
				}
			}
			key := strings.TrimSuffix(strings.TrimPrefix(path, "/chart/"), "/tracks")
			if strings.HasPrefix(path, "/playlist/") {
				key = "pl" + strings.TrimSuffix(strings.TrimPrefix(path, "/playlist/"), "/tracks")
			}
			d.mu.Lock()
			list := d.charts[key]
			d.mu.Unlock()
			var data []map[string]any
			for _, tr := range list {
				data = append(data, map[string]any{
					"id": tr.ID, "title": tr.Title, "title_short": cmpOr(tr.Short, tr.Title),
					"preview": fmt.Sprintf("%s/preview/%d.mp3", d.srv.URL, tr.ID),
					"artist":  map[string]any{"id": 1, "name": tr.Artist},
					"album":   map[string]any{"cover_medium": fmt.Sprintf("%s/cover/%d.jpg", d.srv.URL, tr.ID)},
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data, "total": len(data)})
		case strings.HasPrefix(path, "/preview/"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/preview/"), ".mp3"), 10, 64)
			d.mu.Lock()
			bad := d.failing[id]
			d.mu.Unlock()
			if bad {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(audio)
		case strings.HasPrefix(path, "/cover/"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/cover/"), ".jpg"), 10, 64)
			d.mu.Lock()
			bad := d.noCover[id]
			d.mu.Unlock()
			if bad {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(cover.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fetched is the requests Deezer has had.
func (d *fakeDeezer) fetched() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// client drives a server over pipes, as signalbot would, and checks that
// every line the server writes is a valid JSON-RPC message.
type client struct {
	t      *testing.T
	s      *server
	in     *io.PipeWriter
	notes  chan string
	served chan error
	done   sync.WaitGroup // the reader
	stop   context.CancelFunc

	mu      sync.Mutex
	id      int
	waiting map[string]chan response
	lines   int
	closed  bool
	// judge, if set, answers the server's sampling requests, as the bot's
	// model would, with the text it returns for the request's params.
	judge func(params json.RawMessage) string
}

// rpcIn is a message as the client reads it.
type rpcIn struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// testServer is a server keeping its state in dir, on clk, with its log
// in logs, using Deezer at api; extra settings come from env.
func testServer(t *testing.T, dir string, clk clock, logs io.Writer, api string, env ...string) *server {
	t.Helper()
	vars := map[string]string{"SONGS_STATE_DIR": dir, "SONGS_DEEZER": api, "SONGS_TIME": "30s"} // the tests were written for 30s; TestConfig checks the default
	for i := 0; i+1 < len(env); i += 2 {
		vars[env[i]] = env[i+1]
	}
	cfg, err := loadConfig(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	if vars["gap"] == "" {
		cfg.gap = 0 // songs posted at once; TestGap has the pause
	}
	s, err := newServer(cfg, clk, mrand.New(mrand.NewPCG(1, 2)), http.DefaultClient, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// connect serves s over pipes.
func connect(t *testing.T, s *server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, s: s, in: inW, notes: make(chan string, 16), served: make(chan error, 1), stop: cancel, waiting: map[string]chan response{}}
	c.done.Add(1)
	go func() {
		err := s.srv.Run(ctx, inR, outW)
		outW.Close()
		c.served <- err
	}()
	go func() {
		defer c.done.Done()
		c.read(outR)
	}()
	t.Cleanup(c.close)
	return c
}

// read checks and dispatches each line the server writes.
func (c *client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		c.mu.Lock()
		c.lines++
		c.mu.Unlock()
		var m rpcIn
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&m); err != nil || dec.More() || m.JSONRPC != "2.0" {
			c.t.Errorf("stdout has a line that isn't one JSON-RPC message: %.200s", line)
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			select { // a test that doesn't wait for them mustn't hold up the server
			case c.notes <- m.Method:
			default:
			}
		case m.Method == "sampling/createMessage" && len(m.ID) > 0 && c.judge != nil:
			text := c.judge(m.Params)
			res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"result": map[string]any{"role": "assistant", "content": map[string]string{"type": "text", "text": text}, "model": "x/test-model"}})
			go c.send(string(res))
		case m.Method != "":
			c.t.Errorf("the server asked %s", m.Method)
		case len(m.ID) > 0 && (m.Error == nil) != (len(m.Result) == 0):
			c.mu.Lock()
			ch := c.waiting[string(m.ID)]
			delete(c.waiting, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- response{ID: m.ID, Result: m.Result, Error: m.Error}
			}
		default:
			c.t.Errorf("stdout has a message that is neither a request, a response nor a notification: %.200s", line)
		}
	}
}

// close ends the session as signalbot does, by closing stdin, waits for
// the server to finish, and stops its fetching, as main does.
func (c *client) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.in.Close()
	c.mu.Unlock()
	select {
	case err := <-c.served:
		if err != nil {
			c.t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		c.t.Error("the server didn't stop")
	}
	c.stop()
	c.done.Wait()
	c.s.shutdown()
	c.s.save()
}

// send writes one raw line to the server.
func (c *client) send(line string) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// request sends a request and waits for its response.
func (c *client) request(method string, params any) response {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := strconv.Itoa(c.id)
	n := c.id
	ch := make(chan response, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": n, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(string(data))
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		c.t.Fatalf("no answer to %s", method)
	}
	return response{}
}

// result is a request's result, which must not be an error.
func (c *client) result(method string, params any, v any) {
	c.t.Helper()
	r := c.request(method, params)
	if r.Error != nil {
		c.t.Fatalf("%s: error %d %s", method, r.Error.Code, r.Error.Message)
	}
	if err := json.Unmarshal(r.Result.(json.RawMessage), v); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// initialize is the bot's handshake, offering reply watches as asked, and
// its subscription to the outbox.
func (c *client) initialize(replies bool) {
	c.t.Helper()
	c.initializeWith(replies, false)
}

// initializeWith is initialize, offering reactions on a watch too, as a
// bot at signalbot fe51891 does.
func (c *client) initializeWith(replies, reactions bool) {
	c.t.Helper()
	c.initializeOn(replies, reactions, "signal")
}

// initializeOn is initializeWith from a bot on networks, the first its
// own; on more than one it names them all.
func (c *client) initializeOn(replies, reactions bool, networks ...string) {
	c.t.Helper()
	capability := map[string]any{"version": 1, "network": networks[0], "watchReplies": replies, "watchReactions": reactions}
	if len(networks) > 1 {
		capability["networks"] = networks
	}
	caps := map[string]any{"experimental": map[string]any{vendor + "addon": capability}}
	var init initializeResult
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion, "clientInfo": map[string]string{"name": "signalbot", "version": "1"},
		"capabilities": caps,
	}, &init)
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
}

// from is the context of a message sent now by user in chat, as a direct
// call carries it, with the bot's deadline a minute away.
func from(clk clock, chat, user string) callContext {
	return callContext{Chat: chat, Sender: user, Ref: "r_1", Addressed: true, Group: true, Sent: clk.Now().Unix(), Deadline: time.Now().Add(time.Minute).Unix()}
}

// tool calls a tool as the bot does, with the message's context in _meta.
func (c *client) tool(name string, args map[string]string, ctx callContext) toolResult {
	c.t.Helper()
	if ctx.Network == "" { // a bot on one network: its chats show buttons if it does
		ctx.Buttons = c.s.srv.Buttons()
	}
	var r toolResult
	c.result("tools/call", map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{vendor + "context": ctx},
	}, &r)
	return r
}

func (c *client) song(args string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("song", map[string]string{"args": args}, ctx)
}

// react is a reaction to the game's messages, as the bot passes it.
func (c *client) react(removed bool, ctx callContext) {
	c.t.Helper()
	var r toolResult
	c.result("tools/call", map[string]any{
		"name": "song_answer", "arguments": map[string]any{"reaction": "👍", "removed": removed},
		"_meta": map[string]any{vendor + "context": ctx},
	}, &r)
}

func (c *client) reply(text string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("song_answer", map[string]string{"text": text}, ctx)
}

// outbox is the outbox page after cursor.
func (c *client) outbox(cursor string) outboxPage {
	c.t.Helper()
	var r readResult
	c.result("resources/read", uriParams{outboxURI + "/" + cursor}, &r)
	if len(r.Contents) != 1 || r.Contents[0].MimeType != "application/json" {
		c.t.Fatalf("read %+v", r)
	}
	var p outboxPage
	if err := json.Unmarshal([]byte(r.Contents[0].Text), &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// note waits for a notification.
func (c *client) note() string {
	c.t.Helper()
	select {
	case m := <-c.notes:
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("no notification")
	}
	return ""
}

// ready waits until no song is being fetched, nor a game's songs chosen,
// in the background.
func (c *client) ready() {
	c.t.Helper()
	for range 500 {
		c.s.mu.Lock()
		n := len(c.s.preparing) + len(c.s.gathering)
		c.s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal("a song is still being fetched")
}

// open is the open song.
func (c *client) open() track {
	c.t.Helper()
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	g := c.s.games[chat]
	if g == nil || g.Current >= len(g.Songs) {
		c.t.Fatal("no song open")
	}
	return g.Songs[g.Current]
}

// words is a tool result's text.
func words(r toolResult) string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// recorded is whether r records an answer; it asks for no reaction, as
// reactions take from the chat's post budget.
func recorded(r toolResult) bool { return words(r) == "Recorded." && r.Meta == nil }

const (
	chat  = "c_one"
	alice = "u_alice"
	bob   = "u_bob"
)

// begin is a fake clock, a fake Deezer, a server and a client, with the
// session initialized as a bot that offers reply watches.
func begin(t *testing.T, env ...string) (*fakeClock, *client, *fakeDeezer, *syncBuffer) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL, env...))
	c.initialize(true)
	return clk, c, d, logs
}

// checkSong checks that op posts song i of n, with a clip of the preview:
// an MP3 of whole frames, with no tags.
func checkSong(t *testing.T, op outboxOp, i, n int) {
	t.Helper()
	if op.Op != "post" || op.Chat != chat || op.ReplyTo != "" || op.Text != fmt.Sprintf("🎵 **Song %d of %d** · reply to this message with the title or the artist", i, n) {
		t.Fatalf("song %d: %+v", i, op)
	}
	if op.Audio == nil || op.Audio.MimeType != "audio/mpeg" || len(op.Images) != 0 {
		t.Fatalf("song %d's audio %+v", i, op.Audio)
	}
	b, err := base64.StdEncoding.DecodeString(op.Audio.Data)
	if err != nil || len(b) < 50000 || len(b) > 120000 {
		t.Fatalf("song %d's clip: %d bytes, %v", i, len(b), err)
	}
	if bytes.Contains(b, []byte("ID3")) || bytes.Contains(b, []byte("TAG")) || bytes.Contains(b, []byte("Never Gonna")) {
		t.Errorf("song %d's clip has a tag", i)
	}
	if _, _, ok := parseFrame(b); !ok {
		t.Errorf("song %d's clip doesn't start with a frame", i)
	}
}

func TestInitializeAndTools(t *testing.T) {
	_, c, _, _ := begin(t)
	var init initializeResult
	c.result("initialize", map[string]any{"protocolVersion": "2099-01-01"}, &init)
	if init.ProtocolVersion != protocolVersion || init.Capabilities.Experimental[vendor+"addon"].Version != 1 ||
		!init.Capabilities.Resources.Subscribe || init.ServerInfo.Name != "songs" {
		t.Errorf("initialize %+v", init)
	}
	var list struct {
		Tools []tool `json:"tools"`
	}
	c.result("tools/list", struct{}{}, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if !slices.Equal(tl.Meta.Visibility, []string{"bot"}) || tl.InputSchema.Type != "object" {
			t.Errorf("tool %+v", tl)
		}
	}
	if !slices.Equal(names, []string{"song", "song_answer", "song_press"}) {
		t.Errorf("tools %v", names)
	}
	if r := c.request("tools/call", map[string]any{"name": "nope"}); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool %+v", r)
	}
	if r := c.request("resources/read", uriParams{"file:///etc/passwd"}); r.Error == nil || r.Error.Code != codeNotFound {
		t.Errorf("another resource %+v", r)
	}
	var r toolResult
	c.result("tools/call", map[string]any{"name": "song", "arguments": map[string]string{"args": ""}}, &r)
	if !r.IsError || words(r) != noChat {
		t.Errorf("no context %+v", r)
	}
}

// title is a reply naming t's title, as a player would write it.
func title(t track) string { return titleNames(t)[0] }

// typo is s without its last letter.
func typo(s string) string { r := []rune(s); return string(r[:len(r)-1]) }

// A whole game: the chart, each song a post with its clip, answers of the
// title, the artist and both, the early end, the reveals as replies with
// the cover, the results, the unwatch and the scores; the log names no
// song and no one.
func TestGame(t *testing.T) {
	clk, c, d, logs := begin(t)
	began := clk.Now()
	r := c.song("3", from(clk, chat, alice))
	if r.Meta == nil || r.Meta.Watch == nil {
		t.Fatalf("no watch: %q", words(r))
	}
	w := *r.Meta.Watch
	if w.ID == "" || !w.Replies || w.Pattern != "" || w.Tool != "song_answer" || w.Guard != "" ||
		w.Until != began.Add(3*(30*time.Second+settle)+fetchSlack+watchSlack).Format(time.RFC3339) {
		t.Errorf("watch %+v", w)
	}
	if got := words(r); !strings.HasPrefix(got, "🎵 **Song quiz** · 3 songs, 5s clips, 30s to answer each.\n**Reply** to each clip with the title or the artist") {
		t.Errorf("intro %q", got)
	}
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 1 {
		t.Fatalf("song 1 %+v", page)
	}
	checkSong(t, page.Ops[0], 1, 3)
	s1, song1 := page.Ops[0], c.open()
	if got := words(c.song("", from(clk, chat, bob))); got != "A song quiz is on: song 1 of 3." {
		t.Errorf("a second /song: %q", got)
	}
	c.ready() // song 2, fetched in the background

	clk.Advance(5 * time.Second)
	if r := c.reply(strings.ToUpper(typo(title(song1))), from(clk, chat, alice)); !recorded(r) {
		t.Errorf("Alice's title: %+v", r)
	}
	if r := c.reply("no idea", from(clk, chat, bob)); !recorded(r) {
		t.Errorf("Bob's first answer: %+v", r)
	}
	if r := c.reply("it's "+song1.Artist+"!", from(clk, chat, bob)); !recorded(r) {
		t.Errorf("Bob's artist: %+v", r)
	}
	if r := c.reply(title(song1), from(clk, "c_two", bob)); recorded(r) || words(r) != noGame {
		t.Errorf("an answer in another chat: %+v", r)
	}
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("ops before time is up: %+v", p)
	}

	clk.Advance(20 * time.Second) // 5s left: ⏰ on song 1
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 {
		t.Fatalf("song 1's ⏰ %+v", page)
	}
	checkWarn(t, page.Ops[0], s1.ID)
	clk.Advance(5*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 {
		t.Fatalf("song 1's reveal %+v", page)
	}
	want := "✅ **" + song1.Title + "** — " + song1.Artist + "\nTitle: {u_alice} ⚡\nArtist: {u_bob}"
	if o := page.Ops[0]; o.Op != "post" || o.Chat != chat || o.Text != want || o.ReplyTo != s1.ID || o.Audio != nil {
		t.Errorf("reveal %+v\nwant %q as a reply to %s", o, want, s1.ID)
	}
	if im := page.Ops[0].Images; len(im) != 1 || im[0].MimeType != "image/jpeg" {
		t.Errorf("cover %+v", im)
	} else if b, err := base64.StdEncoding.DecodeString(im[0].Data); err != nil || !bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) {
		t.Errorf("the cover isn't a JPEG: %v", err)
	}
	checkSong(t, page.Ops[1], 2, 3)
	s2, song2 := page.Ops[1], c.open()
	if song2.ID == song1.ID {
		t.Error("a song twice")
	}
	c.ready()

	// Song 2: both answer both, so it ends early.
	clk.Advance(2 * time.Second)
	c.reply(title(song2)+" by "+song2.Artist, from(clk, chat, alice))
	clk.Advance(settle)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Fatalf("song 2 ended before Bob answered: %+v", p)
	}
	c.reply(song2.Artist+" - "+title(song2), from(clk, chat, bob))
	clk.Advance(settle)
	c.note()
	page = c.outbox(page.Cursor)
	want = "✅ **" + song2.Title + "** — " + song2.Artist + "\nTitle: {u_alice} ⚡, {u_bob}\nArtist: {u_alice}, {u_bob}"
	if len(page.Ops) != 2 || page.Ops[0].Text != want || page.Ops[0].ReplyTo != s2.ID {
		t.Fatalf("song 2's early reveal %+v\nwant %q", page, want)
	}
	checkSong(t, page.Ops[1], 3, 3)
	s3 := page.Ops[1]
	c.ready()

	clk.Advance(30*time.Second + settle) // nobody answers
	c.note()
	page = c.outbox(page.Cursor)
	results := "🏁 **Song quiz over**\n🥇 {u_alice} 35 · 2/3 songs\n🥈 {u_bob} 20 · 2/3 songs"
	if len(page.Ops) != 4 || !strings.Contains(page.Ops[1].Text, "\nNobody got it.") || page.Ops[1].ReplyTo != s3.ID ||
		page.Ops[2].Text != results || page.Ops[2].ReplyTo != "" {
		t.Fatalf("results %+v\nwant %q", page, results)
	}
	checkWarn(t, page.Ops[0], s3.ID)
	if o := page.Ops[3]; o.Op != "unwatch" || o.Chat != chat || o.Watch != w.ID {
		t.Errorf("unwatch %+v", o)
	}
	if r := c.reply("anything", from(clk, chat, bob)); recorded(r) || words(r) != noGame {
		t.Errorf("an answer after the game: %+v", r)
	}
	if got := words(c.song("scores", from(clk, chat, bob))); got != "🏆 **Song quiz scores, last 7 days**\n🥇 {u_alice} 35 · 1 game\n🥈 {u_bob} 20 · 1 game" {
		t.Errorf("scores %q", got)
	}
	clk.Advance(8 * 24 * time.Hour)
	if got := words(c.song("scores", from(clk, chat, bob))); !strings.HasPrefix(got, "No song quiz scores") {
		t.Errorf("scores a week later: %q", got)
	}
	if req := d.fetched(); !slices.Contains(req, "/chart/0/tracks?limit=100") || slices.Contains(req, "/genre") {
		t.Errorf("requests %v", req)
	}

	c.close()
	secrets := []string{"u_alice", "u_bob", "c_one", "no idea", "Rhapsod"}
	for _, s := range songs {
		secrets = append(secrets, s.Title, title(s), s.Artist, "/preview/"+strconv.FormatInt(s.ID, 10))
	}
	for _, secret := range secrets {
		if strings.Contains(strings.ToLower(logs.String()), strings.ToLower(secret)) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
	for _, line := range []string{"started: 3 songs, 5s clips, 30s to answer", "song 2 ended early", "ended after 3 of 3 songs: 2 players scored"} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, logs)
		}
	}
}

// An edit replaces an answer while the song is open; an answer after its
// time, or an edit of an older message, doesn't count.
func TestLateAndEdited(t *testing.T) {
	clk, c, _, _ := begin(t)
	c.song("2", from(clk, chat, alice))
	c.ready()
	song := c.open()
	clk.Advance(10 * time.Second)
	first := from(clk, chat, alice)
	c.reply("no idea", first)
	edit := first
	edit.Edited = clk.Now().Add(time.Second).Unix()
	if r := c.reply(song.Artist, edit); !recorded(r) {
		t.Errorf("an edit: %+v", r)
	}
	if r := c.reply("no idea", first); recorded(r) || words(r) != "A later answer of yours counts." {
		t.Errorf("an older version counted: %+v", r)
	}
	old := from(clk, chat, bob)
	old.Sent, old.Edited = clk.Now().Add(-time.Hour).Unix(), clk.Now().Unix()
	if r := c.reply(song.Artist, old); recorded(r) {
		t.Errorf("an edit of an older message counted: %+v", r)
	}
	clk.Advance(21 * time.Second) // past the deadline, before the reveal
	if r := c.reply(song.Artist, from(clk, chat, bob)); recorded(r) || words(r) != tooLate {
		t.Errorf("a late answer: %+v", r)
	}
	ended := words(c.song("end", from(clk, chat, alice)))
	if !strings.Contains(ended, "\nArtist: {u_alice} ⚡\n") || !strings.HasSuffix(ended, "🥇 {u_alice} 10 · 1/1 songs") {
		t.Errorf("end %q", ended)
	}
}

// Only the player who started a game may end it; ending reveals the open
// song and the results at once, and queues the unwatch alone.
func TestEnd(t *testing.T) {
	clk, c, _, _ := begin(t)
	if got := words(c.song("stop", from(clk, chat, alice))); got != noGame {
		t.Errorf("end with no game: %q", got)
	}
	c.song("", from(clk, chat, alice))
	c.ready()
	if got := words(c.song("END", from(clk, chat, bob))); got != "Only the player who started this song quiz can end it." {
		t.Errorf("Bob ends: %q", got)
	}
	song := c.open()
	c.reply(title(song), from(clk, chat, bob))
	ended := words(c.song("end", from(clk, chat, alice)))
	if !strings.HasPrefix(ended, "✅ **"+song.Title+"** — ") || !strings.HasSuffix(ended, "Title: {u_bob} ⚡\n\n🏁 **Song quiz over**\n🥇 {u_bob} 15 · 1/1 songs") {
		t.Errorf("end: %q", ended)
	}
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Audio == nil || page.Ops[1].Op != "unwatch" {
		t.Errorf("after an end: %+v", page)
	}
	clk.Advance(5 * time.Minute) // the game's timer, stopped
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("the timer of an ended game fired: %+v", p)
	}
	c.s.mu.Lock()
	if len(c.s.media) != 0 || len(c.s.games) != 0 {
		t.Errorf("an ended game kept %d media, %d games", len(c.s.media), len(c.s.games))
	}
	c.s.mu.Unlock()
	if got := words(c.song("scores", from(clk, chat, alice))); !strings.Contains(got, "🥇 {u_bob} 15 · 1 game") {
		t.Errorf("scores %q", got)
	}
}

// A preview that fails is skipped for another track; a cover that fails
// leaves the reveal without it; a game whose next song can't be fetched
// ends early; with no preview that works, there's no game.
func TestPreviewFails(t *testing.T) {
	clk, c, d, logs := begin(t)
	r := c.song("rock 2", from(clk, chat, alice))
	if r.Meta == nil {
		t.Fatalf("no game: %q", words(r))
	}
	c.ready()
	c.s.mu.Lock()
	g := c.s.games[chat]
	if g == nil || g.Count != 1 || len(g.Songs) != 1 || g.Songs[0].ID != 201 {
		t.Fatalf("game %+v", g)
	}
	c.s.mu.Unlock()
	clk.Advance(30*time.Second + settle)
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 5 || page.Ops[0].Audio == nil || !strings.HasPrefix(page.Ops[2].Text, "✅ **Back in Black** — AC/DC\nNobody got it.") ||
		len(page.Ops[2].Images) != 0 || page.Ops[3].Text != "🏁 **Song quiz over.** Nobody scored." || page.Ops[4].Op != "unwatch" {
		t.Fatalf("ops %+v", page)
	}
	checkWarn(t, page.Ops[1], page.Ops[0].ID)
	if !strings.Contains(logs.String(), "a track skipped: a preview: status 404") {
		t.Errorf("log:\n%s", logs)
	}

	d.mu.Lock()
	d.failing[201] = true
	d.mu.Unlock()
	if got := words(c.song("rock", from(clk, "c_two", alice))); got != noSongs {
		t.Errorf("no preview works: %q", got)
	}
	if got := words(c.song("r&b", from(clk, "c_two", alice))); got != noSongs {
		t.Errorf("an empty chart: %q", got)
	}
}

// Deezer down, or answering with an error, is said so.
func TestDeezerDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"type":"Exception","message":"Quota limit exceeded","code":4}}`)
	}))
	defer quota.Close()
	for _, api := range []string{down.URL, quota.URL} {
		clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
		c := connect(t, testServer(t, t.TempDir(), clk, io.Discard, api))
		c.initialize(true)
		for _, args := range []string{"", "rock"} {
			if r := c.song(args, from(clk, chat, alice)); words(r) != unreachable || r.Meta != nil {
				t.Errorf("%s %q: %+v", api, args, r)
			}
		}
		c.close()
	}
}

// A game survives a restart: its answers are kept, its timer re-armed, a
// song whose time passed while it was down is revealed at once, and the
// next song is fetched again.
func TestRestart(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	var logs syncBuffer
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
	c.initialize(true)
	c.song("3", from(clk, chat, alice))
	c.ready()
	song1 := c.open()
	clk.Advance(10 * time.Second)
	c.reply(title(song1), from(clk, chat, alice))
	c.close()

	// Back before the deadline, with a new clock: the old timers are gone.
	clk = &fakeClock{now: clk.Now().Add(10 * time.Second)}
	c = connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
	c.initialize(true)
	if got := words(c.song("", from(clk, chat, bob))); got != "A song quiz is on: song 1 of 3." {
		t.Errorf("after a restart: %q", got)
	}
	if r := c.reply(song1.Artist, from(clk, chat, bob)); !recorded(r) {
		t.Errorf("Bob's answer after a restart: %+v", r)
	}
	c.ready()
	c.close()

	// Back after the deadline: song 1 is revealed at once, without its
	// cover, and song 2 is fetched again and posted.
	clk = &fakeClock{now: clk.Now().Add(30 * time.Second)}
	c = connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
	c.initialize(true)
	c.ready()
	page := c.outbox("old.1")
	if len(page.Ops) != 3 {
		t.Fatalf("ops %+v", page)
	}
	checkSong(t, page.Ops[0], 1, 3)
	if o := page.Ops[1]; o.ReplyTo != page.Ops[0].ID || !strings.HasSuffix(o.Text, "\nTitle: {u_alice} ⚡\nArtist: {u_bob}") || len(o.Images) != 0 {
		t.Errorf("song 1's reveal %+v", o)
	}
	checkSong(t, page.Ops[2], 2, 3)
	if !strings.Contains(logs.String(), "1 games open, 1 songs revealed late") {
		t.Errorf("log:\n%s", &logs)
	}
	s2, song2 := page.Ops[2], c.open()
	if song2.ID == song1.ID {
		t.Error("song 1 again")
	}
	clk.Advance(5 * time.Second)
	c.reply(song2.Artist, from(clk, chat, bob))
	c.ready()
	clk.Advance(25*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 3 || page.Ops[1].ReplyTo != s2.ID || len(page.Ops[1].Images) != 1 {
		t.Fatalf("song 2's reveal %+v", page)
	}
	checkWarn(t, page.Ops[0], s2.ID)
	checkSong(t, page.Ops[2], 3, 3)
	c.close()

	// Back after the game's time: its last song is revealed, then the
	// results.
	clk = &fakeClock{now: clk.Now().Add(30 * time.Minute)}
	c = connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
	c.initialize(true)
	page = c.outbox("old.9")
	if n := len(page.Ops); n < 3 || page.Ops[n-1].Op != "unwatch" ||
		page.Ops[n-2].Text != "🏁 **Song quiz over**\n🥇 {u_alice} 15 · 1/3 songs\n🥈 {u_bob} 15 · 2/3 songs" {
		t.Errorf("half an hour on %+v", page)
	}
}

// A song posted in a chat isn't played there again for 30 days; other
// chats are free to play it.
func TestNoRepeats(t *testing.T) {
	clk, c, _, _ := begin(t)
	play := func(n int) {
		t.Helper()
		for range n {
			c.ready()
			clk.Advance(30*time.Second + settle)
		}
		c.s.mu.Lock()
		defer c.s.mu.Unlock()
		if c.s.games[chat] != nil {
			t.Fatal("the game is still on")
		}
	}
	playedIDs := func() []int64 {
		c.s.mu.Lock()
		defer c.s.mu.Unlock()
		var ids []int64
		for _, st := range c.s.played[chat] {
			ids = append(ids, st.ID)
		}
		return ids
	}
	c.song("3", from(clk, chat, alice))
	play(3)
	first := playedIDs()
	if len(first) != 3 {
		t.Fatalf("played %v", first)
	}
	r := c.song("", from(clk, chat, alice)) // 5 of the 5 songs left
	if !strings.Contains(words(r), "· 5 songs,") {
		t.Fatalf("second game %q", words(r))
	}
	play(5)
	all := playedIDs()
	slices.Sort(all)
	if !slices.Equal(all, []int64{101, 102, 103, 104, 105, 106, 107, 108}) {
		t.Errorf("played %v", all)
	}
	if got := words(c.song("", from(clk, chat, alice))); got != allPlayed {
		t.Errorf("a third game: %q", got)
	}
	if r := c.song("2", from(clk, "c_two", alice)); r.Meta == nil {
		t.Errorf("another chat: %q", words(r))
	}
	clk.Advance(31 * 24 * time.Hour)
	if r := c.song("1", from(clk, chat, alice)); r.Meta == nil {
		t.Errorf("a month on: %q", words(r))
	}
}

// With the pause, a song's reveal goes out at once and the next song 5s
// later, with its full time; a reply in between isn't recorded.
func TestGap(t *testing.T) {
	clk, c, _, _ := begin(t, "gap", "on")
	began := clk.Now()
	r := c.song("2", from(clk, chat, alice))
	if w := r.Meta.Watch; w.Until != began.Add(2*(30*time.Second+settle)+gapTime+fetchSlack+watchSlack).Format(time.RFC3339) {
		t.Errorf("watch until %s", w.Until)
	}
	c.ready()
	c.note()
	page := c.outbox("0")
	c.reply(c.open().Artist, from(clk, chat, alice))
	clk.Advance(30*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 || page.Ops[0].Op != "react" || page.Ops[1].ReplyTo == "" {
		t.Fatalf("the ⏰ and the reveal alone %+v", page)
	}
	if rr := c.reply("anything", from(clk, chat, bob)); recorded(rr) || words(rr) != notYet {
		t.Errorf("a reply in the pause: %+v", rr)
	}
	clk.Advance(gapTime)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 {
		t.Fatalf("song 2 %+v", page)
	}
	checkSong(t, page.Ops[0], 2, 2)
	clk.Advance(29 * time.Second) // its time runs from when it was posted
	if rr := c.reply(c.open().Artist, from(clk, chat, bob)); !recorded(rr) {
		t.Errorf("an answer in song 2's time: %+v", rr)
	}
}

// A reveal stays within the bot's 4000 characters with many players right.
func TestLongReveal(t *testing.T) {
	clk, c, _, _ := begin(t)
	c.song("", from(clk, chat, alice))
	c.ready()
	song := c.open()
	for i := range 30 {
		c.reply(title(song)+" by "+song.Artist, from(clk, chat, fmt.Sprintf("u_player%02d", i)))
	}
	clk.Advance(30*time.Second + settle)
	c.note()
	text := c.outbox("0").Ops[2].Text // after the song and its ⏰
	if !strings.Contains(text, "{u_player19} and 10 more\nArtist: ") || len([]rune(text)) > maxPost {
		t.Errorf("reveal %q", text)
	}
}

// Without reply watches (an older bot), /song says what it needs and asks
// Deezer nothing; the scores still work.
func TestNeedsWatch(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, io.Discard, d.srv.URL))
	c.initialize(false)
	if r := c.song("rock", from(clk, chat, alice)); words(r) != needsWatch || r.Meta != nil {
		t.Errorf("%+v", r)
	}
	if got := words(c.song("scores", from(clk, chat, alice))); !strings.HasPrefix(got, "No song quiz scores") {
		t.Errorf("scores %q", got)
	}
	if req := d.fetched(); len(req) != 0 {
		t.Errorf("requests %v", req)
	}
}

// The arguments: a genre, then a clip length and a number of songs, in
// either order, at the end; the genre list is fetched once.
func TestArgs(t *testing.T) {
	clk, c, d, _ := begin(t)
	for i, tc := range []struct{ args, chart, intro string }{
		{"rock", "/chart/152/tracks?limit=100", "🎵 **Song quiz: Rock** · "},
		{"Hip Hop 10s 2", "/chart/116/tracks?limit=100", "🎵 **Song quiz: Rap/Hip Hop** · 2 songs, 10s clips,"},
		{"RAP 2 25s", "/chart/116/tracks?limit=100", "🎵 **Song quiz: Rap/Hip Hop** · 2 songs, 25s clips,"},
		{"3 1s", "/chart/0/tracks?limit=100", "🎵 **Song quiz** · 3 songs, 1s clips,"},
		{"all 2", "/chart/0/tracks?limit=100", "🎵 **Song quiz** · 2 songs,"},
	} {
		ch := "c_args" + strconv.Itoa(i) // a chat each, so no song is a repeat
		before := len(d.fetched())
		r := c.song(tc.args, from(clk, ch, alice))
		if !strings.HasPrefix(words(r), tc.intro) {
			t.Errorf("%q: %q", tc.args, words(r))
		}
		if req := d.fetched()[before:]; !slices.Contains(req, tc.chart) {
			t.Errorf("%q: requests %v", tc.args, req)
		}
		c.ready()
		c.song("end", from(clk, ch, alice))
	}
	for args, want := range map[string]string{
		"11":        badCount,
		"0":         badCount,
		"rock 20":   badCount,
		"31s":       badClip,
		"0s":        badClip,
		"rock 2.5s": badClip,
		"polka":     "I don't know that genre. Try one of: pop, rock, rap/hip hop, r&b, games, or /song genres for them all.",
		"genres":    "🎵 **Genres:** pop, rock, rap/hip hop, r&b, games.\n**Decades:** 60s, 70s, 80s, 90s, 2000s, 2010s, 2020s.\nPlay one with, say, /song rock or /song 80s; plain /song plays the main chart.",
		"GENRE":     "🎵 **Genres:** pop, rock, rap/hip hop, r&b, games.\n**Decades:** 60s, 70s, 80s, 90s, 2000s, 2010s, 2020s.\nPlay one with, say, /song rock or /song 80s; plain /song plays the main chart.",
		"help":      c.s.helpText(false),
	} {
		if got := words(c.song(args, from(clk, chat, alice))); got != want {
			t.Errorf("%q: %q", args, got)
		}
	}
	n := 0
	for _, req := range d.fetched() {
		if req == "/genre" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the genres fetched %d times", n)
	}
}

func TestMarking(t *testing.T) {
	queen := track{Title: "Bohemian Rhapsody (Remastered 2011)", Short: "Bohemian Rhapsody", Artist: "Queen"}
	funk := track{Title: "Uptown Funk (feat. Bruno Mars)", Artist: "Mark Ronson"}
	jude := track{Title: "Hey Jude - Remastered 2015", Artist: "The Beatles"}
	silence := track{Title: "The Sound of Silence", Artist: "Simon & Garfunkel"}
	kiss := track{Title: "One Kiss", Artist: "Calvin Harris"}
	halo := track{Title: "Halo", Artist: "Beyoncé"}
	tw := track{Title: "22", Artist: "Taylor Swift"}
	senorita := track{Title: "Señorita", Artist: "Shawn Mendes & Camila Cabello"}
	roll := track{Title: "Rock & Roll Ain't Noise Pollution", Artist: "AC/DC"}
	feat := track{Title: "Stay ft. Justin Bieber", Artist: "The Kid LAROI feat. Justin Bieber"}
	weeknd := track{Title: "Blinding Lights", Artist: "The Weeknd"}
	flowers := track{Title: "Flowers", Artist: "Miley Cyrus"}
	believe := track{Title: "Believe", Artist: "Cher"}
	stay := track{Title: "Stay", Artist: "Rihanna"}
	one := track{Title: "One", Artist: "U2"}
	x := track{Title: "Marks", Artist: "X"}
	aha := track{Title: "Take On Me", Artist: "a-ha"}
	ewf := track{Title: "September", Artist: "Earth, Wind & Fire"}
	florence := track{Title: "Dog Days Are Over", Artist: "Florence + the Machine"}
	for _, tc := range []struct {
		t             track
		reply         string
		title, artist bool
	}{
		{queen, "Bohemian Rhapsody", true, false},
		{queen, "bohemian rapsody", true, false},
		{queen, "Queen", false, true},
		{queen, "Bohemian Rhapsody by Queen", true, true},
		{queen, "Queen - Bohemian Rapsodi!", true, true},
		{queen, "Bohemian Rhapsody (Remastered 2011)", true, false},
		{queen, "queens, I think", false, true},
		{queen, "no idea", false, false},
		{queen, "", false, false},
		{funk, "Uptown Funk", true, false},
		{funk, "uptown funk feat bruno mars", true, false},
		{funk, "Bruno Mars", false, false},
		{funk, "mark ronson", false, true},
		{jude, "hey jude", true, false},
		{jude, "Hey Jud", true, false},
		{jude, "beatles", false, true},
		{jude, "The Beatles, Hey Jude", true, true},
		{silence, "sound of silence by simon and garfunkel", true, true},
		{silence, "Garfunkel", false, false},
		{silence, "simon & garfunkel", false, true},
		{kiss, "dua lipa", false, false},
		{kiss, "one kis", true, false},
		{kiss, "calvin harris", false, true},
		{halo, "beyonce", false, true},
		{halo, "halo", true, false},
		{halo, "hal", false, false},
		{tw, "22", true, false},
		{tw, "23 by taylor swift", false, true},
		{senorita, "senorita", true, false},
		{senorita, "camila cabelo", false, false},
		{senorita, "shawn mendes and camila cabelo", false, true},
		{roll, "rock and roll aint noise pollution", true, false},
		{roll, "ACDC", false, true},
		{roll, "ac dc", false, true},
		{feat, "stay", true, false},
		{feat, "justin bieber", false, false},
		{feat, "the kid laroi", false, true},
		{queen, "Bohemian Rapsody by Queen", true, true},
		{weeknd, "taylor swift drake the weeknd dua lipa ed sheeran", false, false},
		{weeknd, "the weeknd", false, true},
		{flowers, "stay hello flowers love go yes", false, false},
		{flowers, "flowers by miley", true, false},
		{believe, "do you believe in love cher", true, true}, // long, but names both
		{believe, "do you believe in love today", false, false},
		{believe, "do you believe", true, false},
		{stay, "I'll stay out of this one", false, false},
		{one, "no one knows", false, false},
		{one, "one", true, false},
		{one, "one by u2", true, true},
		{halo, "halo by beyonce", true, true},
		{x, "who is it, x", false, true}, // a short name at the end still counts
		{aha, "ha ha", false, true},      // known: "a-ha" normalises to "ha"
		{ewf, "we didn't start the fire", false, false},
		{ewf, "fire", false, false},
		{ewf, "earth wind and fire", false, true},
		{florence, "machine", false, false},
	} {
		if gt, ga := mark(tc.reply, tc.t); gt != tc.title || ga != tc.artist {
			t.Errorf("%q for %q by %q: title %v, artist %v", tc.reply, tc.t.Title, tc.t.Artist, gt, ga)
		}
	}
	for in, want := range map[string]string{
		"Yesterday (Remastered 2009)":   "Yesterday",
		"(I Can't Get No) Satisfaction": "Satisfaction",
		"(Remix)":                       "(Remix)",
		"Song - Radio Edit":             "Song",
		"Song (Live) (2004 Remaster)":   "Song",
		"Song feat. Someone":            "Song",
		"Daft Punk Is Playing":          "Daft Punk Is Playing",
	} {
		if got := stripVersion(in); got != want {
			t.Errorf("stripVersion(%q) = %q", in, got)
		}
	}
	if got := normalize("The Beatles & Co.!"); got != "beatles and co" {
		t.Errorf("normalize %q", got)
	}
	genres := []genre{{0, "All"}, {132, "Pop"}, {116, "Rap/Hip Hop"}, {165, "R&B"}, {113, "Dance"}}
	for name, want := range map[string]int{"pop": 132, "POP": 132, "hip hop": 116, "hiphop": 116, "rap": 116, "rap/hip hop": 116, "r&b": 165, "R and B": 165, "rnb": 165, "RnB": 165, "dance": 113, "all": 0} {
		if g, ok := findGenre(genres, name); !ok || g.ID != want {
			t.Errorf("findGenre(%q) = %v %v", name, g, ok)
		}
	}
	for _, name := range []string{"", "polka", "the"} {
		if g, ok := findGenre(genres, name); ok {
			t.Errorf("findGenre(%q) = %v", name, g)
		}
	}
}

// Stdout carries only JSON-RPC, whatever arrives on stdin.
func TestStdoutIsProtocol(t *testing.T) {
	_, c, _, _ := begin(t)
	c.send("not json")
	c.send("")
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	c.send(`{"jsonrpc":"2.0","id":99,"result":{}}`)
	c.send(`{"id":"x"` + strings.Repeat(" ", maxLine) + `}`)
	if r := c.request("nope/nope", nil); r.Error == nil || r.Error.Code != codeNoMethod {
		t.Errorf("unknown method %+v", r)
	}
	if r := c.request("tools/call", "not an object"); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("bad params %+v", r)
	}
	c.result("ping", nil, new(struct{}))
	c.close()
	// initialize, subscribe, the parse error, nope/nope, tools/call and ping.
	if c.lines != 6 {
		t.Errorf("%d lines on stdout", c.lines)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	env := func(vars ...string) func(string) string {
		m := map[string]string{"SONGS_STATE_DIR": dir}
		for i := 0; i+1 < len(vars); i += 2 {
			m[vars[i]] = vars[i+1]
		}
		return func(k string) string { return m[k] }
	}
	c, err := loadConfig(env())
	if err != nil || c.Songs != 5 || c.Time != 35*time.Second || c.MemLimit != 64<<20 || c.Deezer != "https://api.deezer.com" || c.gap != gapTime {
		t.Errorf("defaults %+v %v", c, err)
	}
	c, err = loadConfig(env("SONGS_SONGS", "8", "SONGS_TIME", "1m", "SONGS_MEMLIMIT", "1GiB", "SONGS_DEEZER", "http://127.0.0.1:9/"))
	if err != nil || c.Songs != 8 || c.Time != time.Minute || c.MemLimit != 1<<30 || c.Deezer != "http://127.0.0.1:9" {
		t.Errorf("set %+v %v", c, err)
	}
	for _, bad := range [][]string{
		{"SONGS_STATE_DIR", ""}, {"SONGS_SONGS", "11"}, {"SONGS_SONGS", "x"}, {"SONGS_TIME", "5s"}, {"SONGS_TIME", "3m"},
		{"SONGS_MEMLIMIT", "1MiB"}, {"SONGS_DEEZER", "ftp://api.deezer.com"}, {"SONGS_DEEZER", "api.deezer.com"},
	} {
		if _, err := loadConfig(env(bad...)); err == nil || !strings.Contains(err.Error(), bad[0]) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if err := checkDir(dir + "/missing/songs"); err != nil {
		t.Errorf("a missing directory isn't created: %v", err)
	} else if fi, err := os.Stat(dir + "/missing/songs"); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory made: %v %v", fi, err)
	}
	if err := checkDir("/proc/songs"); err == nil {
		t.Error("a directory that can't be made")
	}
}

// Deezer's previews and covers are fetched only over https, unless the API
// itself is http (a test's); a preview over the limit is refused.
func TestFetchLimits(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxPreview+1))
	}))
	defer big.Close()
	d := newDeezer("https://api.deezer.com", big.Client())
	if _, err := d.Preview(context.Background(), track{Preview: big.URL + "/x.mp3"}); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("http with an https API: %v", err)
	}
	d = newDeezer(big.URL, big.Client())
	if _, err := d.Preview(context.Background(), track{Preview: big.URL + "/x.mp3"}); err == nil || !strings.Contains(err.Error(), "over") || strings.Contains(err.Error(), big.URL) {
		t.Errorf("a big preview: %v", err)
	}
	if _, err := d.Preview(context.Background(), track{Preview: "file:///etc/passwd"}); err == nil {
		t.Error("a file URL")
	}
}

func TestNoRedirectToHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("audio"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok.mp3" {
			w.Write([]byte("audio"))
			return
		}
		http.Redirect(w, r, plain.URL+"/x.mp3", http.StatusFound)
	}))
	defer secure.Close()
	client := secure.Client()
	d := newDeezer(secure.URL, client)
	if _, err := d.get(context.Background(), "preview", secure.URL+"/x.mp3", 100, time.Second); err == nil || !strings.Contains(err.Error(), "can't connect") || strings.Contains(err.Error(), plain.URL) {
		t.Errorf("a redirect to http: %v", err)
	}
	if body, err := d.get(context.Background(), "preview", secure.URL+"/ok.mp3", 100, time.Second); err != nil || string(body) != "audio" {
		t.Errorf("an https fetch: %q, %v", body, err)
	}
	if client.CheckRedirect != nil {
		t.Error("the shared client was changed")
	}
}

// A chart with fewer new songs than asked for plays what it has, and the
// intro says so.
func TestFewerThanAsked(t *testing.T) {
	clk, c, _, _ := begin(t)
	got := words(c.song("hip hop 5", from(clk, chat, alice)))
	if !strings.Contains(got, " · 2 songs (that's all the chart has that hasn't been played here lately, of the 5 asked for), ") {
		t.Errorf("intro %q", got)
	}
}

// /song help names every command and the defaults in use.
func TestHelp(t *testing.T) {
	clk, c, _, _ := begin(t)
	got := words(c.song("help", from(clk, chat, alice)))
	for _, want := range []string{"/song [genre or decade] [clip length] [rounds]", "/song 80s 10s 3", "/song genres", "/song start", "/song wait", "/song end", "/song scores", "Title 10 pts"} {
		if !strings.Contains(got, want) {
			t.Errorf("help lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n") + 1; n > 6 {
		t.Errorf("help is %d lines", n)
	}
}

// /song 80s plays the editors' 80s Hits playlist, not 80-second clips; it
// combines with a clip length; with the pinned playlist gone, the search
// finds the editors' own "80s Hits"; an unknown decade is no genre.
func TestDecade(t *testing.T) {
	clk, c, d, _ := begin(t)
	got := words(c.song("80s 10s 2", from(clk, chat, alice)))
	if !strings.HasPrefix(got, "🎵 **Song quiz: 80s** · 2 songs, 10s clips") {
		t.Fatalf("intro %q", got)
	}
	if !slices.Contains(d.fetched(), "/playlist/867825522/tracks?limit=100") {
		t.Errorf("requests %v", d.fetched())
	}
	c.song("end", from(clk, chat, alice))
	d.mu.Lock()
	delete(d.charts, "pl867825522")
	d.mu.Unlock()
	if got := words(c.song("1980s", from(clk, "c_two", bob))); !strings.HasPrefix(got, "🎵 **Song quiz: 80s** · ") {
		t.Errorf("after the search %q", got)
	}
	if !slices.Contains(d.fetched(), "/playlist/868/tracks?limit=100") {
		t.Errorf("the search didn't pick the editors' 80s Hits: %v", d.fetched())
	}
	for name, want := range map[string]string{"80s": "80s", "80's": "80s", "1980s": "80s", "eighties": "80s", "2000s": "2000s", "00s": "2000s", "2010s": "2010s", "1960s": "60s", "70's": "70s", "2020s": "2020s", "20's": "2020s"} {
		if dec, ok := findDecade(name); !ok || dec.Label != want {
			t.Errorf("findDecade(%q) = %v %v", name, dec, ok)
		}
	}
	// 10s alone is 10-second clips of the main chart; the 2010s are 2010s.
	if got := words(c.song("10s", from(clk, "c_three", alice))); !strings.HasPrefix(got, "🎵 **Song quiz** · ") || !strings.Contains(got, ", 10s clips") {
		t.Errorf("/song 10s: %q", got)
	}
	if got := words(c.song("20s", from(clk, "c_six", alice))); !strings.HasPrefix(got, "🎵 **Song quiz** · ") || !strings.Contains(got, ", 20s clips") {
		t.Errorf("/song 20s: %q", got)
	}
	if got := words(c.song("2020s", from(clk, "c_seven", alice))); !strings.HasPrefix(got, "🎵 **Song quiz: 2020s** · ") {
		t.Errorf("/song 2020s: %q", got)
	}
	if got := words(c.song("2010s 10s", from(clk, "c_four", alice))); !strings.HasPrefix(got, "🎵 **Song quiz: 2010s** · ") || !strings.Contains(got, ", 10s clips") {
		t.Errorf("/song 2010s 10s: %q", got)
	}
	for _, name := range []string{"50s", "80", "rock"} {
		if _, ok := findDecade(name); ok {
			t.Errorf("findDecade(%q) passed", name)
		}
	}
}

// In a group, with a bot that passes reactions, players join by reacting
// to the intro; song 1 comes 30s after the last join once there are 2;
// fewer than 2 after 5 minutes calls the game off; a DM starts at once.
func TestLobby(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
	c.initializeWith(true, true)
	r := c.song("2", from(clk, chat, alice))
	c.ready()
	if !strings.Contains(words(r), "React to this message with any emoji to join") || !strings.Contains(words(r), "whoever started it too") || !r.Meta.Watch.Reactions || !strings.HasSuffix(r.Meta.Watch.Over, "is over. Start another with /song.") {
		t.Fatalf("intro %+v", r)
	}
	if rr := c.reply("anything", from(clk, chat, bob)); words(rr) != "The song quiz hasn't started yet." {
		t.Errorf("an answer in the lobby: %q", words(rr))
	}
	c.react(false, from(clk, chat, alice)) // the starter joins too, but alone doesn't start it
	clk.Advance(lobbyWait)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("started with one player: %+v", p)
	}
	clk.Advance(5 * time.Second)
	c.react(false, from(clk, chat, bob))
	clk.Advance(29 * time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("a song before the lobby ended: %+v", p)
	}
	clk.Advance(time.Second)
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Audio == nil {
		t.Fatalf("song 1 %+v", page)
	}
	c.s.mu.Lock()
	if p := c.s.games[chat].Players; !slices.Equal(p, []string{alice, bob}) {
		t.Errorf("players %v", p)
	}
	c.s.mu.Unlock()

	c.song("2", from(clk, "c_two", alice))
	clk.Advance(lobbyMax)
	c.note()
	p := c.outbox(page.Cursor) // the first chat's game goes on meanwhile; the call-off is last
	if n := len(p.Ops); n < 2 || p.Ops[n-2].Chat != "c_two" || p.Ops[n-2].Text != "Not enough players joined, so the song quiz is off." || p.Ops[n-1].Op != "unwatch" {
		t.Errorf("call-off %+v", p)
	}
	dm := from(clk, "c_dm", alice)
	dm.Group = false
	if got := words(c.song("2", dm)); strings.Contains(got, "React to") {
		t.Errorf("a DM has a lobby: %q", got)
	}
}

// In a DM the starter is the one player: a song ends as soon as they
// answer, from song 1.
func TestDMEndsEarly(t *testing.T) {
	clk, c, _, _ := begin(t)
	dm := from(clk, "c_dm", alice)
	dm.Group = false
	c.song("2", dm)
	c.note()
	page := c.outbox("0")
	c.reply("no idea", dm)
	clk.Advance(settle)
	c.note()
	if p := c.outbox(page.Cursor); len(p.Ops) == 0 || p.Ops[0].ReplyTo == "" {
		t.Errorf("song 1 waited for its timer: %+v", p)
	}
}

// With the bot's model, the model judges each answer at the reveal: its
// verdicts replace the fuzzy match's, both ways; the answers it gets are
// numbered in the order sent; without a usable reply, the fuzzy match
// stands.
func TestJudge(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
	var mu sync.Mutex
	var asked []string
	reply := `{"answers":[{"id":1,"title":true,"artist":true},{"id":2,"title":false,"artist":false}]}`
	c.judge = func(params json.RawMessage) string {
		var p sampleRequest
		json.Unmarshal(params, &p)
		if strings.Contains(p.SystemPrompt, "sung mainly in English") { // choosing the songs: all are
			return englishAll(p.Messages[0].Content.Text)
		}
		mu.Lock()
		asked = append(asked, p.Messages[0].Content.Text)
		defer mu.Unlock()
		return reply
	}
	caps := map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReplies": true}}, "sampling": map[string]any{}}
	c.result("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": caps}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	dm := func(user string) callContext { ctx := from(clk, chat, user); ctx.Group = false; return ctx }
	c.song("2", dm(alice))
	waitFor(t, c.s, "song 1", func() bool { g := c.s.games[chat]; return g != nil && g.Post != "" })
	c.s.mu.Lock()
	song := c.s.games[chat].Songs[0]
	c.s.mu.Unlock()
	c.reply("something misspelt", dm(bob))            // the model says right
	c.reply(song.Title+" by "+song.Artist, dm(alice)) // the fuzzy match says right; the model, wrong
	clk.Advance(settle)
	waitFor(t, c.s, "the reveal", func() bool { g := c.s.games[chat]; return g == nil || g.Current == 1 })
	mu.Lock()
	if len(asked) != 1 || !strings.Contains(asked[0], `"answers":[{"id":1,"text":"something misspelt"},{"id":2,"text":"`) || strings.Contains(asked[0], `"game"`) {
		t.Errorf("asked %q", asked)
	}
	mu.Unlock()
	page := c.outbox("0")
	var reveal string
	for _, o := range page.Ops {
		if o.ReplyTo != "" {
			reveal = o.Text
		}
	}
	if !strings.Contains(reveal, "Title: {u_bob} ⚡") || !strings.Contains(reveal, "Artist: {u_bob}") || strings.Contains(reveal, "{u_alice}") || strings.Contains(reveal, "🎮") {
		t.Errorf("reveal %q", reveal)
	}
	for _, secret := range []string{"misspelt", song.Title} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
	if v, err := parseVerdicts("```json\n" + reply + "\n```"); err != nil || len(v) != 2 || !v[0].Artist {
		t.Errorf("parseVerdicts %v %v", v, err)
	}
	if _, err := parseVerdicts("sure!"); err == nil {
		t.Error("no JSON parsed")
	}
}

// waitFor waits, up to 5s, until ok holds under s.mu.
func waitFor(t *testing.T, s *server, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		done := ok()
		s.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s", what)
		}
	}
}

// englishAll is the model's answer to which tracks are in English: all of
// those it was given.
func englishAll(list string) string {
	var items []struct {
		ID int `json:"id"`
	}
	json.Unmarshal([]byte(list), &items)
	var ids []int
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	b, _ := json.Marshal(map[string][]int{"english": ids})
	return string(b)
}

// Only songs in English are played: the model picks them, as Deezer
// doesn't say a song's language; without the model, a title or artist in
// another script is dropped; SONGS_ANY_LANGUAGE plays any.
func TestEnglishOnly(t *testing.T) {
	ts := []track{{ID: 1, Title: "Hello", Artist: "Adele"}, {ID: 2, Title: "Despacito", Artist: "Luis Fonsi"}, {ID: 3, Title: "다이너마이트", Artist: "BTS"}}
	s := &server{cfg: config{}, log: log.New(io.Discard, "", 0)}
	s.srv = addon.New(addon.Config{Name: "songs"}) // no sampling offered: by script
	if got := s.english(context.Background(), chat, ts, false); len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("by script %v", got)
	}
	s.cfg.Anyway = true
	if got := s.english(context.Background(), chat, ts, false); len(got) != 3 {
		t.Errorf("any language %v", got)
	}
}

// Films/Games is shown as games, found by game or video games, and played
// as video game music.
func TestGamesGenre(t *testing.T) {
	genres := []genre{{0, "All"}, {132, "Pop"}, {173, "Films/Games"}, {197, "Latin Music"}}
	if got := genreNames(genres, 10); got != "pop, games, latin" {
		t.Errorf("names %q", got)
	}
	for _, name := range []string{"games", "game", "video games", "videogame"} {
		if g, ok := findGenre(genres, name); !ok || g.ID != gamesGenre {
			t.Errorf("findGenre(%q) = %v %v", name, g, ok)
		}
	}
	if g, ok := findGenre(genres, "latin"); !ok || g.ID != 197 || genreLabel(g) != "Latin" {
		t.Errorf("latin: %v %v", g, ok)
	}
	if genreLabel(genres[2]) != "Games" {
		t.Error("label")
	}
	// Without the model, games keeps the tracks it can't judge, even with
	// SONGS_ANY_LANGUAGE.
	s := &server{cfg: config{Anyway: true}, log: log.New(io.Discard, "", 0)}
	s.srv = addon.New(addon.Config{Name: "songs"})
	if got := s.english(context.Background(), chat, []track{{ID: 1, Title: "Halo Theme", Artist: "Martin O'Donnell"}}, true); len(got) != 1 {
		t.Errorf("games without the model %v", got)
	}
}

// /song games plays the editors' game playlists, not the Films/Games
// chart, and is shown as games.
func TestGamesSource(t *testing.T) {
	clk, c, d, _ := begin(t)
	got := words(c.song("games 2", from(clk, chat, alice)))
	if !strings.HasPrefix(got, "🎵 **Song quiz: Games** · ") {
		t.Fatalf("intro %q", got)
	}
	if !slices.Contains(d.fetched(), "/playlist/4380597982/tracks?limit=100") || slices.Contains(d.fetched(), "/chart/173/tracks?limit=100") {
		t.Errorf("requests %v", d.fetched())
	}
}

// gameTracks are video game tracks: two name their game, two don't.
var gameTracks = []track{
	{ID: 301, Title: "Halo Theme", Artist: "Martin O'Donnell"},
	{ID: 302, Title: "Sweden", Artist: "C418"},
	{ID: 303, Title: "Minecraft (Main Theme)", Short: "Minecraft", Artist: "C418"},
	{ID: 304, Title: "Song of Storms", Artist: "Koji Kondo"},
}

// gameOf is the fake model's game for each of gameTracks.
var gameOf = map[string]string{"Halo Theme": "Halo", "Sweden": "Minecraft", "Minecraft (Main Theme)": "Minecraft", "Song of Storms": "The Legend of Zelda: Ocarina of Time"}

// With the model, /song games keeps only tracks whose title or artist
// names their game, which the model names; the game counts as the title,
// the judge is told it, and the reveal names it.
func TestGamesNamed(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	d.mu.Lock()
	d.charts["pl4380597982"] = gameTracks
	d.mu.Unlock()
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
	var mu sync.Mutex
	var asked []string
	c.judge = func(params json.RawMessage) string {
		var p sampleRequest
		json.Unmarshal(params, &p)
		if strings.Contains(p.SystemPrompt, "video game music") { // choosing the songs: each with its game
			var items []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			}
			json.Unmarshal([]byte(p.Messages[0].Content.Text), &items)
			var out []map[string]any
			for _, it := range items {
				out = append(out, map[string]any{"id": it.ID, "game": gameOf[it.Title]})
			}
			b, _ := json.Marshal(map[string]any{"english": out})
			return string(b)
		}
		mu.Lock()
		asked = append(asked, p.Messages[0].Content.Text)
		mu.Unlock()
		return `{"answers":[{"id":1,"title":true,"artist":false}]}`
	}
	caps := map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReplies": true}}, "sampling": map[string]any{}}
	c.result("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": caps}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	dm := from(clk, chat, alice)
	dm.Group = false
	c.song("games 2", dm)
	waitFor(t, c.s, "song 1", func() bool { g := c.s.games[chat]; return g != nil && g.Post != "" })
	c.s.mu.Lock()
	g := c.s.games[chat]
	var ids []int64
	for _, s := range append(slices.Clone(g.Songs), g.Pool...) {
		ids = append(ids, s.ID)
		if s.Game != gameOf[s.Title] {
			t.Errorf("%s: game %q", s.Title, s.Game)
		}
	}
	song := g.Songs[0]
	c.s.mu.Unlock()
	if len(ids) == 0 || slices.ContainsFunc(ids, func(id int64) bool { return id != 301 && id != 303 }) {
		t.Errorf("songs %v", ids)
	}
	if !strings.Contains(logs.String(), "songs of video games named after their game: 2 of 4") {
		t.Errorf("log %q", logs.String())
	}
	c.reply(song.Game, dm)
	clk.Advance(settle)
	waitFor(t, c.s, "the reveal", func() bool { g := c.s.games[chat]; return g == nil || g.Current == 1 })
	mu.Lock()
	if len(asked) != 1 || !strings.Contains(asked[0], `"game":"`+song.Game+`"`) {
		t.Errorf("asked %q", asked)
	}
	mu.Unlock()
	var reveal string
	for _, o := range c.outbox("0").Ops {
		if o.ReplyTo != "" {
			reveal = o.Text
		}
	}
	if !strings.Contains(reveal, "— "+song.Artist+" · 🎮 "+song.Game+"\n") || !strings.Contains(reveal, "Title: {u_alice} ⚡") {
		t.Errorf("reveal %q", reveal)
	}
	for _, secret := range []string{"Sweden", "Halo Theme"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

// A game's name scores the title, without the model too; a track without
// a game is marked as before; saved tracks keep their game, and ones saved
// before games had one load.
func TestGameAnswers(t *testing.T) {
	sweden := track{Title: "Sweden", Artist: "C418", Game: "Minecraft"}
	if title, artist := mark("minecraft", sweden); !title || artist {
		t.Errorf("the game: %v %v", title, artist)
	}
	if title, artist := mark("minecraft by c418", sweden); !title || !artist {
		t.Errorf("the game and artist: %v %v", title, artist)
	}
	dragonborn := track{Title: "Dragonborn", Artist: "Jeremy Soule", Game: "The Elder Scrolls V: Skyrim"}
	if title, artist := mark("dragonborn halo zelda mario sonic tetris doom jeremy soule", dragonborn); title || artist {
		t.Errorf("a list of guesses, with a long game: %v %v", title, artist)
	}
	if title, artist := mark("elder scrolls skyrim by jeremy soule", dragonborn); !title || !artist {
		t.Errorf("a long game and artist: %v %v", title, artist)
	}
	sweden.Game = ""
	if title, _ := mark("minecraft", sweden); title {
		t.Error("a game without one")
	}
	if got := titleNames(sweden); !slices.Equal(got, []string{"Sweden"}) {
		t.Errorf("titleNames %v", got)
	}
	g := &game{Songs: []track{sweden}, Answers: map[string]answer{alice: {Text: "x"}}}
	if req := judgeRequest(g); strings.Contains(req.Messages[0].Content.Text, `"game"`) {
		t.Errorf("a game in %s", req.Messages[0].Content.Text)
	}
	var old track
	if err := json.Unmarshal([]byte(`{"id":1,"title":"Sweden","artist":"C418","preview":"p"}`), &old); err != nil || old.Game != "" || old.Title != "Sweden" {
		t.Errorf("old track %+v %v", old, err)
	}
	b, _ := json.Marshal(track{ID: 1, Title: "Sweden", Game: "Minecraft"})
	var back track
	if json.Unmarshal(b, &back); back.Game != "Minecraft" {
		t.Errorf("round trip %s", b)
	}
	if b, _ := json.Marshal(old); strings.Contains(string(b), "game") {
		t.Errorf("no game saved as %s", b)
	}
}

// lobby is a fake clock, a fake Deezer whose charts are held, and a
// client whose bot passes reactions, so a group's game has a lobby.
func lobby(t *testing.T) (*fakeClock, *client, *fakeDeezer, *syncBuffer) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	d.block()
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
	c.initializeWith(true, true)
	return clk, c, d, logs
}

// asked waits until Deezer has had a request for a chart.
func (d *fakeDeezer) asked(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if slices.ContainsFunc(d.fetched(), func(u string) bool { return strings.HasPrefix(u, "/chart/") }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no chart asked for")
		}
	}
}

// With a lobby, /song answers with the intro at once, while Deezer holds
// the chart, and its watch covers the lobby and the songs; songs chosen
// during the lobby, song 1 comes as the countdown ends.
func TestLobbyAtOnce(t *testing.T) {
	clk, c, d, _ := lobby(t)
	start := clk.Now()
	r := c.song("2", from(clk, chat, alice))
	if !strings.Contains(words(r), "React to this message") || r.Meta == nil || r.Meta.Watch == nil || !r.Meta.Watch.Reactions {
		t.Fatalf("intro %+v", r)
	}
	want := start.Add(lobbyMax + 2*(30*time.Second+settle) + fetchSlack + watchSlack).Format(time.RFC3339)
	if r.Meta.Watch.Until != want {
		t.Errorf("until %s, want %s", r.Meta.Watch.Until, want)
	}
	d.asked(t)
	if words(c.reply("anything", from(clk, chat, bob))) != notStarted {
		t.Error("an answer while the songs are chosen")
	}
	d.release()
	c.ready()
	c.react(false, from(clk, chat, alice))
	c.react(false, from(clk, chat, bob))
	clk.Advance(lobbyWait)
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 1 {
		t.Fatalf("ops %+v", page)
	}
	checkSong(t, page.Ops[0], 1, 2)
}

// When the lobby is ready before the songs, one post says so, and song 1
// comes as soon as they are.
func TestLobbyReadyFirst(t *testing.T) {
	clk, c, d, _ := lobby(t)
	c.song("2", from(clk, chat, alice))
	d.asked(t)
	c.react(false, from(clk, chat, alice))
	c.react(false, from(clk, chat, bob))
	clk.Advance(lobbyWait)
	page := c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Text != gettingReady || page.Ops[0].Audio != nil {
		t.Fatalf("ops %+v", page)
	}
	if words(c.reply("anything", from(clk, chat, bob))) != notStarted {
		t.Error("an answer while the songs are chosen")
	}
	clk.Advance(time.Minute) // the time it takes
	d.release()
	waitFor(t, c.s, "song 1", func() bool { g := c.s.games[chat]; return g != nil && g.Post != "" })
	p := c.outbox(page.Cursor)
	if len(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	}
	checkSong(t, p.Ops[0], 1, 2)
	c.s.mu.Lock()
	g := c.s.games[chat]
	if !slices.Equal(g.Players, []string{alice, bob}) || !g.Deadline.Equal(clk.Now().Add(30*time.Second)) {
		t.Errorf("players %v, deadline %v", g.Players, g.Deadline)
	}
	c.s.mu.Unlock()
}

// When no songs can be chosen, the game posts why, as /song would have
// answered, and ends its watch.
func TestLobbyNoSongs(t *testing.T) {
	clk, c, d, logs := lobby(t)
	d.mu.Lock()
	d.charts["0"] = nil
	d.mu.Unlock()
	c.song("2", from(clk, chat, alice))
	c.react(false, from(clk, chat, alice))
	d.release()
	waitFor(t, c.s, "the call-off", func() bool { return c.s.games[chat] == nil })
	page := c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Text != noSongs || page.Ops[1].Op != "unwatch" || page.Ops[1].Watch == "" {
		t.Fatalf("ops %+v", page)
	}
	if !strings.Contains(logs.String(), "no songs to play") {
		t.Errorf("log:\n%s", logs)
	}
	clk.Advance(lobbyMax)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("after the call-off %+v", p)
	}
}

// A lobby called off, by its starter or for want of players, stops
// choosing its songs, and nothing more is posted.
func TestLobbyOffStopsChoosing(t *testing.T) {
	for _, by := range []string{"end", "players"} {
		t.Run(by, func(t *testing.T) {
			clk, c, d, _ := lobby(t)
			c.song("2", from(clk, chat, alice))
			d.asked(t)
			if by == "end" {
				if got := words(c.song("end", from(clk, chat, alice))); got != "The song quiz is off." {
					t.Fatalf("end %q", got)
				}
			} else {
				clk.Advance(lobbyMax)
			}
			c.ready()
			for deadline := time.Now().Add(5 * time.Second); d.gaveUp() == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the chart's request wasn't given up")
				}
			}
			done := make(chan struct{})
			go func() { c.s.bg.Wait(); close(done) }() // the choosing has seen the game is off
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the choosing didn't stop")
			}
			c.close()
			ops := c.s.srv.Outbox()
			last := ops[len(ops)-1].Op
			if last.Op != "unwatch" || len(ops) > 2 {
				t.Errorf("ops %+v", ops)
			}
			if slices.ContainsFunc(d.fetched(), func(u string) bool { return strings.HasPrefix(u, "/preview/") }) {
				t.Error("a preview fetched")
			}
			d.release()
		})
	}
}

// A restart while the songs are chosen keeps the game, lobby or players,
// and chooses them again only once the bot is back.
func TestRestartChoosing(t *testing.T) {
	for _, lobbyOver := range []bool{false, true} {
		t.Run(fmt.Sprint("lobby over ", lobbyOver), func(t *testing.T) {
			clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
			dir := t.TempDir()
			var logs syncBuffer
			d := newFakeDeezer(t)
			d.block()
			c := connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
			c.initializeWith(true, true)
			c.song("2", from(clk, chat, alice))
			d.asked(t)
			c.react(false, from(clk, chat, alice))
			if lobbyOver {
				c.react(false, from(clk, chat, bob))
				clk.Advance(lobbyWait)
				page := c.outbox("0")
				if len(page.Ops) != 1 || page.Ops[0].Text != gettingReady {
					t.Fatalf("ops %+v", page)
				}
				c.outbox(page.Cursor) // the bot has it
			}
			c.close()
			d.release()
			asked := len(d.fetched())

			clk = &fakeClock{now: clk.Now().Add(10 * time.Second)}
			c = connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
			c.s.mu.Lock()
			g := c.s.games[chat]
			if g == nil || g.Lobby == lobbyOver || !g.Preparing || !slices.Contains(g.Joined, alice) {
				t.Fatalf("after the restart %+v", g)
			}
			c.s.mu.Unlock()
			time.Sleep(100 * time.Millisecond) // time enough to ask Deezer, were it not waiting for the bot
			if got := len(d.fetched()); got != asked {
				t.Fatalf("%d requests before the bot was back", got-asked)
			}
			c.initializeWith(true, true)
			c.ready()
			if !lobbyOver {
				c.react(false, from(clk, chat, bob))
				clk.Advance(lobbyWait)
			}
			c.note()
			page := c.outbox("0") // no second post that the songs are coming
			if len(page.Ops) != 1 {
				t.Fatalf("ops %+v", page)
			}
			checkSong(t, page.Ops[0], 1, 2)
		})
	}
}

// In a DM, and in a group whose bot can't pass reactions, /song still
// chooses the songs first: it answers with song 1 queued.
func TestNoLobbyChoosesFirst(t *testing.T) {
	for _, reactions := range []bool{true, false} {
		clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
		d := newFakeDeezer(t)
		c := connect(t, testServer(t, t.TempDir(), clk, io.Discard, d.srv.URL))
		c.initializeWith(true, reactions)
		ctx := from(clk, chat, alice)
		ctx.Group = !reactions // a DM, or a group without a lobby
		r := c.song("2", ctx)
		if strings.Contains(words(r), "React to") || r.Meta == nil || r.Meta.Watch.Reactions {
			t.Errorf("intro %+v", r)
		}
		page := c.outbox("0")
		if len(page.Ops) != 1 {
			t.Fatalf("ops %+v", page)
		}
		checkSong(t, page.Ops[0], 1, 2)
		c.s.mu.Lock()
		if g := c.s.games[chat]; g == nil || g.Preparing || len(c.s.gathering) != 0 {
			t.Errorf("game %+v", g)
		}
		c.s.mu.Unlock()
		c.close()
	}
}

// checkWarn checks that op reacts ⏰ to the post with the op ID post.
func checkWarn(t *testing.T, op outboxOp, post string) {
	t.Helper()
	checkWarnWith(t, op, post, "⏰")
}

// checkWarnWith is checkWarn with the reaction emoji.
func checkWarnWith(t *testing.T, op outboxOp, post, emoji string) {
	t.Helper()
	if op.Op != "react" || op.Chat != chat || op.Post != post || post == "" || op.Emoji != emoji || op.Ref != "" || op.Text != "" {
		t.Errorf("%s %+v, want on %s", emoji, op, post)
	}
}

// Telegram takes only some emoji as reactions, and not ⏰: there the
// warning is 😱. The chat's network is its Context's when the bot is on
// several, else the bot's.
func TestWarnTelegram(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks []string
		chat     string // the Context's network
		want     string
	}{
		{"telegram", []string{"telegram"}, "", "😱"},
		{"signal", []string{"signal"}, "", "⏰"},
		{"a telegram chat", []string{"signal", "telegram"}, "telegram", "😱"},
		{"a signal chat", []string{"telegram", "signal"}, "signal", "⏰"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
			d := newFakeDeezer(t)
			c := connect(t, testServer(t, t.TempDir(), clk, &syncBuffer{}, d.srv.URL))
			c.initializeOn(true, false, tc.networks...)
			ctx := from(clk, chat, alice)
			ctx.Network = tc.chat
			c.song("2", ctx)
			c.note()
			page := c.outbox("0")
			s1 := page.Ops[0]
			c.ready()
			c.s.mu.Lock()
			network := c.s.games[chat].Network
			c.s.mu.Unlock()
			if want := cmp.Or(tc.chat, tc.networks[0]); network != want {
				t.Errorf("the game's network %q, want %q", network, want)
			}
			clk.Advance(25 * time.Second)
			c.note()
			page = c.outbox(page.Cursor)
			if len(page.Ops) != 1 {
				t.Fatalf("song 1's warning %+v", page)
			}
			checkWarnWith(t, page.Ops[0], s1.ID, tc.want)
		})
	}
}

// reacts are the ⏰ reactions on page.
func reacts(page outboxPage) []outboxOp {
	var out []outboxOp
	for _, o := range page.Ops {
		if o.Op == "react" {
			out = append(out, o)
		}
	}
	return out
}

// warned reports whether chat's game has its ⏰ armed.
func (c *client) warned(chat string) bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.s.warns[chat] != nil
}

// 5 seconds before each song's deadline the bot reacts ⏰ to its post, by
// the post's op ID: not to a song that ended early, and each next song
// gets its own.
func TestWarn(t *testing.T) {
	clk, c, _, _ := begin(t)
	c.song("3", from(clk, chat, alice))
	c.note()
	page := c.outbox("0")
	s1 := page.Ops[0]
	c.ready()
	c.reply(title(c.open()), from(clk, chat, alice))
	c.reply("no idea", from(clk, chat, bob))
	clk.Advance(25*time.Second - time.Nanosecond)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Fatalf("ops before 5s are left: %+v", p)
	}
	clk.Advance(time.Nanosecond)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 {
		t.Fatalf("song 1's ⏰ %+v", page)
	}
	checkWarn(t, page.Ops[0], s1.ID)
	clk.Advance(5*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 || len(reacts(page)) != 0 {
		t.Fatalf("song 1's reveal %+v", page)
	}
	s2 := page.Ops[1]
	c.ready()

	// Song 2: Alice and Bob both answer, so it ends early: no ⏰.
	clk.Advance(2 * time.Second)
	c.reply("no idea", from(clk, chat, alice))
	c.reply("no idea", from(clk, chat, bob))
	if c.warned(chat) {
		t.Error("song 2's ⏰ still armed after it ended early")
	}
	clk.Advance(settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 || page.Ops[0].ReplyTo != s2.ID || len(reacts(page)) != 0 {
		t.Fatalf("song 2's early reveal %+v", page)
	}
	s3 := page.Ops[1]
	c.ready()

	// Song 3 gets its own ⏰.
	clk.Advance(25 * time.Second)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 {
		t.Fatalf("song 3's ⏰ %+v", page)
	}
	checkWarn(t, page.Ops[0], s3.ID)
	if s3.ID == s1.ID {
		t.Error("the same post twice")
	}
}

// /song end stops the ⏰.
func TestWarnEnd(t *testing.T) {
	clk, c, _, _ := begin(t)
	c.song("2", from(clk, chat, alice))
	c.ready()
	clk.Advance(10 * time.Second)
	if !c.warned(chat) {
		t.Fatal("no ⏰ armed")
	}
	c.song("end", from(clk, chat, alice))
	c.note()
	page := c.outbox("0")
	if c.warned(chat) {
		t.Error("the ⏰ is still armed after /song end")
	}
	clk.Advance(time.Minute)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 || len(reacts(page)) != 0 {
		t.Errorf("ops after /song end: %+v %+v", page, p)
	}
}

// After a restart with more than 5s left, the ⏰ is armed again; with 5s
// or less, there's none.
func TestWarnRestart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration // into song 1, of 30s
		warn  bool
	}{{"10s left", 20 * time.Second, true}, {"4s left", 26 * time.Second, false}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
			dir := t.TempDir()
			var logs syncBuffer
			d := newFakeDeezer(t)
			c := connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
			c.initialize(true)
			c.song("2", from(clk, chat, alice))
			c.note()
			s1 := c.outbox("0").Ops[0]
			c.ready()
			c.close()

			// Back with a new clock: the old timers are gone.
			clk = &fakeClock{now: clk.Now().Add(tc.after)}
			c = connect(t, testServer(t, dir, clk, &logs, d.srv.URL))
			c.initialize(true)
			c.ready()
			if c.warned(chat) != tc.warn {
				t.Fatalf("⏰ armed: %v, want %v", c.warned(chat), tc.warn)
			}
			before := c.outbox("old.1")
			if len(reacts(before)) != 0 {
				t.Fatalf("a ⏰ at once: %+v", before)
			}
			if !tc.warn {
				// Up to just before the reveal: still no ⏰.
				clk.Advance(30*time.Second - tc.after - time.Second)
				if page := c.outbox(before.Cursor); len(reacts(page)) != 0 {
					t.Errorf("a ⏰ with 4s left: %+v", page)
				}
				return
			}
			clk.Advance(30*time.Second - warnBefore - tc.after)
			page := c.outbox(before.Cursor)
			c.note()
			if len(page.Ops) != 1 {
				t.Fatalf("song 1's ⏰ after a restart %+v", page)
			}
			checkWarn(t, page.Ops[0], s1.ID)
		})
	}
}

// A song shorter than warnMin gets no ⏰: there's nothing useful to warn
// of. The game never posts one (minTime is warnMin), so this checks the
// guard directly.
func TestWarnShort(t *testing.T) {
	clk, c, _, _ := begin(t)
	c.song("2", from(clk, chat, alice))
	c.note()
	page := c.outbox("0")
	c.ready()
	c.s.mu.Lock()
	g := c.s.games[chat]
	g.Deadline = g.Opened.Add(warnMin - time.Second)
	c.s.arm(g)
	armed := c.s.warns[chat] != nil
	c.s.mu.Unlock()
	if armed {
		t.Error("a ⏰ armed for a 9s song")
	}
	clk.Advance(warnMin - time.Second - warnBefore)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("ops on a 9s song: %+v", p)
	}
}

// initializeButtons is the handshake of a bot whose network shows
// buttons, with reply and reaction watches.
func (c *client) initializeButtons() {
	c.t.Helper()
	caps := map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "network": "telegram", "watchReplies": true, "watchReactions": true, "buttons": true}}}
	c.result("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": caps}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
}

// press is a press of the button with data, as the bot passes it.
func (c *client) press(data string, ctx callContext) string {
	c.t.Helper()
	return words(c.tool("song_press", map[string]string{"press": data}, ctx))
}

// buttonLobby is a server whose bot shows buttons, with a lobby opened by
// alice in chat, its songs chosen; it returns the intro too.
func buttonLobby(t *testing.T, block bool) (*fakeClock, *client, *fakeDeezer, *syncBuffer, toolResult) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	d := newFakeDeezer(t)
	if block {
		d.block()
	}
	c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
	c.initializeButtons()
	r := c.song("2", from(clk, chat, alice))
	if !block {
		c.ready()
	}
	return clk, c, d, logs, r
}

// songPosts are the posts with a clip among ops.
func songPosts(ops []outboxOp) int {
	n := 0
	for _, op := range ops {
		if op.Audio != nil {
			n++
		}
	}
	return n
}

// lobbyRow is the lobby's one row of buttons for the game id with n
// joined and its songs, and ▶️ (Start) once 2 have joined.
func lobbyRow(id string, n, songs int) [][]addon.Button {
	row := []addon.Button{
		{Label: fmt.Sprintf("✅ %d", n), Data: "ready:" + id},
		{Label: fmt.Sprintf("Songs: %d", songs), Data: "t1:" + id},
	}
	if n >= 2 {
		row = append(row, addon.Button{Label: "▶️", Data: "start:" + id})
	}
	return [][]addon.Button{append(row, addon.Button{Label: "✖️", Data: "cancel:" + id})}
}

// buttonsOps are the buttons ops in the outbox after cursor, checked to be
// for the lobby id in chat and to leave its text be, and the cursor after
// them.
func (c *client) buttonsOps(cursor, id string) ([][][]addon.Button, string) {
	c.t.Helper()
	rows, texts, next := c.buttonsOpsText(cursor, id)
	for i, text := range texts {
		if text != "" {
			c.t.Errorf("buttons op %d with text %q", i, text)
		}
	}
	return rows, next
}

// buttonsOpsText are the buttons ops in the outbox after cursor, checked
// to be for the lobby id in chat: their rows, their texts, and the cursor
// after them.
func (c *client) buttonsOpsText(cursor, id string) ([][][]addon.Button, []string, string) {
	c.t.Helper()
	page := c.outbox(cursor)
	var rows [][][]addon.Button
	var texts []string
	for _, op := range page.Ops {
		if op.Op != "buttons" {
			continue
		}
		if op.Chat != chat || op.Watch != id {
			c.t.Errorf("buttons op %+v", op)
		}
		rows = append(rows, op.Buttons)
		texts = append(texts, op.Text)
	}
	return rows, texts, page.Cursor
}

// The lobby's buttons are one row: ✅ with none joined, the songs
// toggle and ✖️, no ▶️; only in a group and only where the
// bot shows buttons. Its intro never says to react.
func TestLobbyButtons(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if want := lobbyRow(id, 0, 2); !reflect.DeepEqual(r.Meta.Buttons, want) || r.Meta.OnPress != "song_press" {
		t.Fatalf("buttons %+v, onPress %q", r.Meta.Buttons, r.Meta.OnPress)
	}
	if len(r.Meta.Buttons) != 1 || len(r.Meta.Buttons[0]) != 3 {
		t.Errorf("rows %+v", r.Meta.Buttons)
	}
	w := words(r)
	for _, want := range []string{"Press ✅ to join", "the starter presses ▶️ when everyone's in", "change the number of songs with Songs", "✖️ calls it off"} {
		if !strings.Contains(w, want) {
			t.Errorf("intro without %q: %q", want, w)
		}
	}
	for _, not := range []string{"React", "react", "+30s", "/song wait", "30 seconds after", "✋", "Start", "Cancel", "Ready", "clip length"} {
		if strings.Contains(w, not) {
			t.Errorf("intro with %q: %q", not, w)
		}
	}
	if !r.Meta.Watch.Reactions {
		t.Error("no reactions")
	}
	// The watch covers double the songs, chosen again at the lobby's end.
	want := clk.Now().Add(lobbyMax + prepMax + 4*(30*time.Second+settle) + fetchSlack + watchSlack).Format(time.RFC3339)
	if r.Meta.Watch.Until != want {
		t.Errorf("until %s, want %s", r.Meta.Watch.Until, want)
	}
	dm := from(clk, "c_dm", alice)
	dm.Group = false
	if r := c.song("2", dm); r.Meta == nil || r.Meta.Buttons != nil || r.Meta.OnPress != "" {
		t.Errorf("a DM has buttons: %+v", r.Meta)
	}

	clk2 := &fakeClock{now: clk.Now()}
	d := newFakeDeezer(t)
	c2 := connect(t, testServer(t, t.TempDir(), clk2, &syncBuffer{}, d.srv.URL))
	c2.initializeWith(true, true)
	r2 := c2.song("2", from(clk2, chat, alice))
	c2.ready()
	if r2.Meta == nil || r2.Meta.Buttons != nil || r2.Meta.OnPress != "" || !strings.Contains(words(r2), "begin now with /song start, wait 30s longer with /song wait") ||
		!strings.Contains(words(r2), "React to this message with any emoji to join") || strings.Contains(words(r2), "Ready") {
		t.Errorf("buttons without the capability: %+v %q", r2.Meta, words(r2))
	}
}

// joined is who has joined the lobby in chat.
func (c *client) joined() []string {
	c.t.Helper()
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return slices.Clone(c.s.games[chat].Joined)
}

// ✅ (Ready): anyone's, the starter's too; each join, by a press or a
// reaction, and each leave, replaces the row with the new count, ▶️
// in it from 2; no countdown starts the game; under an older lobby, after
// the start and after the end, a press answers as the other buttons do.
func TestPressReady(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if got := c.press("ready:"+id, from(clk, chat, bob)); got != "You're in: 1 joined." {
		t.Errorf("bob's press: %q", got)
	}
	rows, cur := c.buttonsOps("0", id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], lobbyRow(id, 1, 2)) {
		t.Fatalf("after bob: %+v", rows)
	}
	if got := c.press("ready:"+id, from(clk, chat, bob)); got != alreadyIn {
		t.Errorf("bob again: %q", got)
	}
	c.react(false, from(clk, chat, bob)) // a reaction after it is no second join
	if got := c.joined(); !slices.Equal(got, []string{bob}) {
		t.Errorf("joined %v", got)
	}
	if got := c.press("ready:sother", from(clk, chat, alice)); got != gameOver {
		t.Errorf("an older lobby's press: %q", got)
	}
	if rows, _ := c.buttonsOps(cur, id); len(rows) != 0 {
		t.Errorf("ops without a change: %+v", rows)
	}
	clk.Advance(10 * time.Second)
	if got := c.press("ready:"+id, from(clk, chat, alice)); got != "You're in: 2 joined." {
		t.Errorf("alice's press: %q", got)
	}
	rows, cur = c.buttonsOps(cur, id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], lobbyRow(id, 2, 2)) || len(rows[0]) != 1 {
		t.Fatalf("after alice: %+v", rows)
	}
	c.s.mu.Lock()
	data, _ := json.Marshal(c.s.games[chat])
	c.s.mu.Unlock()
	if !strings.Contains(string(data), `"joined":["`+bob+`","`+alice+`"]`) {
		t.Errorf("not saved: %s", data)
	}
	clk.Advance(lobbyMax - 11*time.Second) // no countdown
	if p := c.outbox(cur); songPosts(p.Ops) != 0 || len(p.Ops) != 0 {
		t.Fatalf("started by itself: %+v", p)
	}
	c.react(true, from(clk, chat, bob)) // bob leaves: no ▶️
	rows, cur = c.buttonsOps(cur, id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], lobbyRow(id, 1, 2)) {
		t.Fatalf("after bob left: %+v", rows)
	}
	c.react(false, from(clk, chat, bob))
	rows, cur = c.buttonsOps(cur, id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], lobbyRow(id, 2, 2)) {
		t.Fatalf("after bob's reaction: %+v", rows)
	}
	if got := c.press("start:"+id, from(clk, chat, alice)); got != startingNow {
		t.Fatalf("start: %q", got)
	}
	page := c.outbox(cur)
	if len(page.Ops) != 1 {
		t.Fatalf("ops %+v", page)
	}
	checkSong(t, page.Ops[0], 1, 2)
	c.s.mu.Lock()
	if g := c.s.games[chat]; g.Lobby || !slices.Equal(g.Players, []string{alice, bob}) {
		t.Errorf("game %+v", g)
	}
	c.s.mu.Unlock()
	if got := c.press("ready:"+id, from(clk, chat, "u_carol")); got != alreadyBegun {
		t.Errorf("after the start: %q", got)
	}
	c.song("end", from(clk, chat, alice))
	for _, data := range []string{"ready:", "wait:", "t1:"} {
		if got := c.press(data+id, from(clk, chat, "u_carol")); got != gameOver {
			t.Errorf("%s after the end: %q", data, got)
		}
	}
}

// A lobby with buttons is called off lobbyMax after it opened, whoever
// joined: with too few, as without buttons; with enough, as nobody
// pressed ▶️.
func TestButtonLobbyCallOff(t *testing.T) {
	for _, tc := range []struct {
		joined []string
		why    string
	}{
		{[]string{bob}, "Not enough players joined, so the song quiz is off."},
		{[]string{bob, alice}, noStartText},
	} {
		clk, c, _, _, r := buttonLobby(t, false)
		id := r.Meta.Watch.ID
		for _, u := range tc.joined {
			c.press("ready:"+id, from(clk, chat, u))
		}
		_, cur := c.buttonsOps("0", id)
		clk.Advance(lobbyMax - time.Second)
		if p := c.outbox(cur); len(p.Ops) != 0 {
			t.Fatalf("ops %+v", p)
		}
		clk.Advance(time.Second)
		p := c.outbox(cur)
		if len(p.Ops) != 2 || p.Ops[0].Text != tc.why || p.Ops[1].Op != "unwatch" || p.Ops[1].Watch != id {
			t.Fatalf("%v: ops %+v", tc.joined, p)
		}
		c.s.mu.Lock()
		if c.s.games[chat] != nil {
			t.Error("the game goes on")
		}
		c.s.mu.Unlock()
		c.close()
	}
}

// game is chat's game, copied.
func (c *client) game() game {
	c.t.Helper()
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	g := c.s.games[chat]
	if g == nil {
		c.t.Fatal("no game")
	}
	return *g
}

// clipLen is how long an MP3 clip plays.
func clipLen(t *testing.T, data string) time.Duration {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	var total time.Duration
	for _, f := range frames(b) {
		total += f.dur
	}
	return total
}

// The songs toggle: the starter's only; it flips between what the lobby
// opened with and double it, replacing the row and saving; songs chosen
// for fewer are chosen again; the game that starts plays what it shows,
// with the clip length /song gave. An older lobby's clip toggle is gone.
func TestPressToggles(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if got := c.press("t1:"+id, from(clk, chat, bob)); got != notStarter {
		t.Errorf("bob's t1: %q", got)
	}
	for _, who := range []string{alice, bob} {
		if got := c.press("t2:"+id, from(clk, chat, who)); got != buttonGone {
			t.Errorf("%s's t2: %q", who, got)
		}
	}
	if rows, _ := c.buttonsOps("0", id); len(rows) != 0 {
		t.Errorf("ops after bob's: %+v", rows)
	}
	if got := c.press("t1:"+id, from(clk, chat, alice)); got != "Songs: 4" {
		t.Errorf("t1: %q", got)
	}
	c.ready()
	if g := c.game(); g.Asked != 4 || g.Count != 4 || g.Gathered != 4 || g.Preparing {
		t.Errorf("4 songs: %+v", g)
	}
	if got := c.press("t1:"+id, from(clk, chat, alice)); got != "Songs: 2" {
		t.Errorf("t1 back: %q", got)
	}
	if g := c.game(); g.Asked != 2 || g.Count != 2 || g.Gathered != 4 || g.Preparing { // the 4 chosen do
		t.Errorf("back to 2: %+v", g)
	}
	if got := c.press("t1:"+id, from(clk, chat, alice)); got != "Songs: 4" {
		t.Errorf("t1 again: %q", got)
	}
	if g := c.game(); g.Count != 4 || g.Preparing {
		t.Errorf("4 again: %+v", g)
	}
	if g := c.game(); g.Clip != 5*time.Second || g.Asked != 4 {
		t.Errorf("after t2: %+v", g)
	}
	rows, texts, cur := c.buttonsOpsText("0", id)
	want := [][][]addon.Button{lobbyRow(id, 0, 4), lobbyRow(id, 0, 2), lobbyRow(id, 0, 4)}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows %+v", rows)
	}
	// Each toggle rewrites the intro for the songs it shows.
	for i, n := range []int{4, 2, 4} {
		if i >= len(texts) {
			break
		}
		if want := fmt.Sprintf(" · %d songs, ", n); !strings.Contains(texts[i], want) || !strings.Contains(texts[i], "Press ✅ to join") {
			t.Errorf("text %d without %q: %q", i, want, texts[i])
		}
	}
	if !strings.Contains(words(r), " · 2 songs, ") {
		t.Errorf("intro %q", words(r))
	}
	c.s.mu.Lock()
	data, _ := json.Marshal(c.s.games[chat])
	c.s.mu.Unlock()
	if !strings.Contains(string(data), `"asked":4`) || !strings.Contains(string(data), `"clip":5000000000`) {
		t.Errorf("not saved: %s", data)
	}
	c.press("ready:"+id, from(clk, chat, bob))
	if got := c.press("start:"+id, from(clk, chat, alice)); got != startingNow {
		t.Fatalf("start: %q", got)
	}
	page := c.outbox(cur)
	var song *outboxOp
	for i := range page.Ops {
		if page.Ops[i].Audio != nil {
			song = &page.Ops[i]
		}
	}
	if song == nil || song.Text != songText(&game{Count: 4}) {
		t.Fatalf("ops %+v", page)
	}
	if d := clipLen(t, song.Audio.Data); d < 5*time.Second || d > 5*time.Second+30*time.Millisecond {
		t.Errorf("a clip of %s", d)
	}
}

// The toggle stops at the game's limit: double, or the limit if less; at
// the limit already, it has one mode, and a press changes nothing. The
// clip length stays what /song gave, up to 30s.
func TestPressTogglesCap(t *testing.T) {
	for _, tc := range []struct {
		args       string
		t1         string // the pop-up
		songs, max int
		clip       time.Duration
	}{
		{"8 20s", "Songs: 10", 8, maxSongs, 20 * time.Second},
		{"10 30s", atMost, 10, 10, maxClip},
	} {
		clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
		d := newFakeDeezer(t)
		c := connect(t, testServer(t, t.TempDir(), clk, &syncBuffer{}, d.srv.URL))
		c.initializeButtons()
		r := c.song(tc.args, from(clk, chat, alice))
		c.ready()
		id := r.Meta.Watch.ID
		if got := c.press("t1:"+id, from(clk, chat, alice)); got != tc.t1 {
			t.Errorf("%s: t1 %q", tc.args, got)
		}
		if got := c.press("t2:"+id, from(clk, chat, alice)); got != buttonGone {
			t.Errorf("%s: t2 %q", tc.args, got)
		}
		c.ready()
		g := c.game()
		if g.Asked != tc.max || g.Clip != tc.clip {
			t.Errorf("%s: %d songs, %s clips", tc.args, g.Asked, g.Clip)
		}
		rows, _, _ := c.buttonsOpsText("0", id)
		if tc.t1 == atMost && len(rows) != 0 {
			t.Errorf("%s: ops %+v", tc.args, rows)
		}
		c.press("t1:"+id, from(clk, chat, alice))
		if g := c.game(); g.Asked != tc.songs {
			t.Errorf("%s: back to %d songs", tc.args, g.Asked)
		}
		c.ready()
		c.close()
	}
}

// A toggle while the songs are still chosen chooses them again, for what
// it shows; the choosing it stopped changes nothing.
func TestPressTogglePreparing(t *testing.T) {
	clk, c, d, logs, r := buttonLobby(t, true)
	id := r.Meta.Watch.ID
	d.asked(t)
	if got := c.press("t1:"+id, from(clk, chat, alice)); got != "Songs: 4" {
		t.Fatalf("t1: %q", got)
	}
	d.release()
	c.ready()
	if g := c.game(); g.Preparing || g.Count != 4 || g.Gathered != 4 || len(g.Songs) != 1 {
		t.Errorf("game %+v", g)
	}
	if strings.Contains(logs.String(), "no songs") {
		t.Errorf("called off: %s", logs)
	}
}

// A run that reaches its deadline just as a toggle chooses the songs again
// leaves the new run alone.
func TestToggleAtDeadline(t *testing.T) {
	clk, c, d, logs, r := buttonLobby(t, true)
	id := r.Meta.Watch.ID
	d.asked(t)
	c.s.mu.Lock()
	run, g := c.s.gathering[id], c.s.games[chat]
	src, clip := g.source(), g.Clip
	c.s.mu.Unlock()
	if got := c.press("t1:"+id, from(clk, chat, alice)); got != "Songs: 4" {
		t.Fatalf("t1: %q", got)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c.s.gathered(ctx, run, chat, id, src, 2, clip) // the old run, waking at its deadline
	c.s.mu.Lock()
	_, running := c.s.gathering[id]
	c.s.mu.Unlock()
	if g := c.game(); !g.Preparing || g.Asked != 4 || !running {
		t.Fatalf("game %+v, running %v", g, running)
	}
	if strings.Contains(logs.String(), "no songs") {
		t.Fatalf("called off: %s", logs)
	}
	d.release()
	c.ready()
	if g := c.game(); g.Preparing || g.Count != 4 || g.Gathered != 4 {
		t.Errorf("game %+v", g)
	}
}

// ✅ presses racing ▶️ start the game once, with every join
// made before it among its players.
func TestPressReadyRaces(t *testing.T) {
	for range 20 {
		clk, c, _, logs, r := buttonLobby(t, false)
		id := r.Meta.Watch.ID
		c.press("ready:"+id, from(clk, chat, alice))
		c.press("ready:"+id, from(clk, chat, bob))
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); c.press("start:"+id, from(clk, chat, alice)) }()
		got := c.press("ready:"+id, from(clk, chat, "u_carol"))
		wg.Wait()
		if got != "You're in: 3 joined." && got != alreadyBegun {
			t.Errorf("press %q", got)
		}
		if p := c.outbox("0"); songPosts(p.Ops) != 1 {
			t.Fatalf("song posts %+v", p)
		}
		if n := strings.Count(logs.String(), "began:"); n != 1 {
			t.Fatalf("began %d times", n)
		}
		c.s.mu.Lock()
		g := c.s.games[chat]
		if want := len(g.Joined); g.Lobby || len(g.Players) != want || (got == alreadyBegun) == slices.Contains(g.Players, "u_carol") {
			t.Errorf("game %+v after %q", g, got)
		}
		c.s.mu.Unlock()
		c.close()
	}
}

// ▶️ (Start): the starter's only; their press is their join; with too few
// it waits; with enough the game starts at once; after it, and under an
// older lobby, a press does nothing.
func TestPressStart(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if got := c.press("start:"+id, from(clk, chat, bob)); got != notStarter {
		t.Errorf("bob's press: %q", got)
	}
	if got := c.press("start:"+id, from(clk, chat, alice)); got != needReady {
		t.Errorf("alone: %q", got)
	}
	if got := c.joined(); !slices.Equal(got, []string{alice}) {
		t.Errorf("joined %v", got)
	}
	rows, cur := c.buttonsOps("0", id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], lobbyRow(id, 1, 2)) {
		t.Fatalf("after alice's start: %+v", rows)
	}
	if got := c.press("start:sother", from(clk, chat, alice)); got != gameOver {
		t.Errorf("an older lobby's press: %q", got)
	}
	c.react(false, from(clk, chat, bob))
	_, cur = c.buttonsOps(cur, id)
	clk.Advance(5 * time.Second)
	if got := c.press("start:"+id, from(clk, chat, alice)); got != startingNow {
		t.Errorf("with 2: %q", got)
	}
	page := c.outbox(cur)
	if len(page.Ops) != 1 {
		t.Fatalf("ops %+v", page)
	}
	checkSong(t, page.Ops[0], 1, 2)
	c.s.mu.Lock()
	if g := c.s.games[chat]; g.Lobby || !slices.Equal(g.Players, []string{alice, bob}) || !g.Started.Equal(clk.Now()) {
		t.Errorf("game %+v", g)
	}
	c.s.mu.Unlock()
	for _, data := range []string{"start:", "cancel:", "wait:", "t1:"} {
		if got := c.press(data+id, from(clk, chat, alice)); got != alreadyBegun {
			t.Errorf("%s after the start: %q", data, got)
		}
	}
	clk.Advance(lobbyWait) // the lobby's timer doesn't start it again
	if p := c.outbox(page.Cursor); songPosts(p.Ops) != 0 {
		t.Errorf("started twice: %+v", p)
	}
	c.song("end", from(clk, chat, alice))
	if got := c.press("start:"+id, from(clk, chat, alice)); got != gameOver {
		t.Errorf("after the end: %q", got)
	}
}

// Typed, /song start does what ▶️ does, in the buttons' words; /song
// wait says there's no countdown.
func TestTypedButtonLobby(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if got := words(c.song("start", from(clk, chat, alice))); got != needReady {
		t.Errorf("alone: %q", got)
	}
	if got := words(c.song("wait", from(clk, chat, alice))); got != noCountdown {
		t.Errorf("wait: %q", got)
	}
	if got := c.press("wait:"+id, from(clk, chat, alice)); got != noCountdown {
		t.Errorf("an old lobby's wait: %q", got)
	}
	c.press("ready:"+id, from(clk, chat, bob))
	rows, cur := c.buttonsOps("0", id)
	if !reflect.DeepEqual(rows, [][][]addon.Button{lobbyRow(id, 1, 2), lobbyRow(id, 2, 2)}) {
		t.Errorf("rows %+v", rows)
	}
	if got := words(c.song("start", from(clk, chat, alice))); got != startingNow {
		t.Errorf("start: %q", got)
	}
	if p := c.outbox(cur); songPosts(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	}
}

// ▶️ while the songs are still chosen starts the game as the timer
// would: a post says they are being made ready.
func TestPressStartPreparing(t *testing.T) {
	clk, c, d, _, r := buttonLobby(t, true)
	d.asked(t)
	c.react(false, from(clk, chat, alice))
	c.react(false, from(clk, chat, bob))
	_, cur := c.buttonsOps("0", r.Meta.Watch.ID)
	if got := c.press("start:"+r.Meta.Watch.ID, from(clk, chat, alice)); got != startingNow {
		t.Fatalf("press %q", got)
	}
	page := c.outbox(cur)
	if len(page.Ops) != 1 || page.Ops[0].Text != gettingReady {
		t.Fatalf("ops %+v", page)
	}
	d.release()
	waitFor(t, c.s, "song 1", func() bool { g := c.s.games[chat]; return g != nil && g.Post != "" })
	if p := c.outbox(page.Cursor); len(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	} else {
		checkSong(t, p.Ops[0], 1, 2)
	}
}

// ✖️ (Cancel): the starter's only; it calls the lobby off as /song stop
// does, saying so in the chat.
func TestPressCancel(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	id := r.Meta.Watch.ID
	if got := c.press("cancel:"+id, from(clk, chat, bob)); got != notStarter {
		t.Errorf("bob's press: %q", got)
	}
	if got := c.press("cancel:"+id, from(clk, chat, alice)); got != cancelled {
		t.Errorf("press %q", got)
	}
	page := c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Text != lobbyOff || page.Ops[1].Op != "unwatch" || page.Ops[1].Watch != id {
		t.Fatalf("ops %+v", page)
	}
	c.s.mu.Lock()
	if c.s.games[chat] != nil {
		t.Error("the game goes on")
	}
	c.s.mu.Unlock()
	if got := c.press("start:"+id, from(clk, chat, alice)); got != gameOver {
		t.Errorf("after the cancel: %q", got)
	}
	clk.Advance(lobbyMax)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("after the cancel %+v", p)
	}
}

// signalLobby is a server whose bot shows no buttons, with a lobby opened
// by alice in chat, its songs chosen; it returns the intro too.
func signalLobby(t *testing.T) (*fakeClock, *client, toolResult) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, &syncBuffer{}, d.srv.URL))
	c.initializeWith(true, true)
	r := c.song("2", from(clk, chat, alice))
	c.ready()
	return clk, c, r
}

// Without buttons, /song wait (or an older lobby's ⏱ +30s): the starter's
// only; each adds 30s, which later joins keep; never past the call-off;
// it is saved.
func TestPressWait(t *testing.T) {
	clk, c, r := signalLobby(t)
	id := r.Meta.Watch.ID
	if got := c.press("wait:"+id, from(clk, chat, bob)); got != notStarter {
		t.Errorf("bob's press: %q", got)
	}
	c.react(false, from(clk, chat, alice))
	c.react(false, from(clk, chat, bob)) // armed for lobbyWait from now
	if got := c.press("wait:"+id, from(clk, chat, alice)); got != waitLonger {
		t.Errorf("press %q", got)
	}
	if got := words(c.song("wait", from(clk, chat, alice))); got != waitLonger {
		t.Errorf("typed %q", got)
	}
	clk.Advance(lobbyWait + 50*time.Second)
	c.react(false, from(clk, chat, "u_carol")) // a later join keeps the extra minute
	clk.Advance(lobbyWait + 59*time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("started before its wait: %+v", p)
	}
	c.s.mu.Lock()
	data, _ := json.Marshal(c.s.games[chat])
	c.s.mu.Unlock()
	if !strings.Contains(string(data), `"extra":60000000000`) {
		t.Errorf("not saved: %s", data)
	}
	clk.Advance(time.Second)
	if p := c.outbox("0"); len(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	}
}

// /song wait stops at the lobby's call-off.
func TestPressWaitCap(t *testing.T) {
	clk, c, _ := signalLobby(t)
	start := clk.Now()
	c.react(false, from(clk, chat, alice))
	c.react(false, from(clk, chat, bob))
	got, n := "", 0
	for ; n < 100; n++ {
		if got = words(c.song("wait", from(clk, chat, alice))); got != waitLonger {
			break
		}
	}
	if got != noLonger || n != int((lobbyMax-lobbyWait)/waitStep) {
		t.Fatalf("after %d: %q", n, got)
	}
	if got := words(c.song("wait", from(clk, chat, alice))); got != noLonger {
		t.Errorf("again: %q", got)
	}
	c.s.mu.Lock()
	at := startAt(c.s.games[chat])
	c.s.mu.Unlock()
	if !at.Equal(start.Add(lobbyMax)) {
		t.Errorf("starts at %v, want %v", at, start.Add(lobbyMax))
	}
	clk.Advance(lobbyMax - time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("ops %+v", p)
	}
	clk.Advance(time.Second)
	if p := c.outbox("0"); songPosts(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	}
}

// The typed /song start, /song wait and /song stop do what the buttons
// do, with the same rules, where the network has no buttons.
func TestTypedLobby(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, &syncBuffer{}, d.srv.URL))
	c.initializeWith(true, true)
	if got := words(c.song("start", from(clk, chat, alice))); got != noGame {
		t.Errorf("no game: %q", got)
	}
	r := c.song("2", from(clk, chat, alice))
	c.ready()
	if r.Meta.Buttons != nil {
		t.Errorf("buttons on Signal: %+v", r.Meta.Buttons)
	}
	for _, cmd := range []string{"start", "wait"} {
		if got := words(c.song(cmd, from(clk, chat, bob))); got != notStarter {
			t.Errorf("bob's %s: %q", cmd, got)
		}
	}
	if got := words(c.song("stop", from(clk, chat, bob))); got != "Only the player who started this song quiz can end it." {
		t.Errorf("bob's stop: %q", got)
	}
	if got := words(c.song("start", from(clk, chat, alice))); got != needPlayers {
		t.Errorf("alone: %q", got)
	}
	c.react(false, from(clk, chat, bob))
	if got := words(c.song("wait", from(clk, chat, alice))); got != waitLonger {
		t.Errorf("wait: %q", got)
	}
	clk.Advance(lobbyWait)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("didn't wait: %+v", p)
	}
	if got := words(c.song("START", from(clk, chat, alice))); got != startingNow {
		t.Errorf("start: %q", got)
	}
	if p := c.outbox("0"); songPosts(p.Ops) != 1 {
		t.Fatalf("ops %+v", p)
	}
	for _, cmd := range []string{"start", "wait"} {
		if got := words(c.song(cmd, from(clk, chat, alice))); got != alreadyBegun {
			t.Errorf("%s after the start: %q", cmd, got)
		}
	}
	c.song("end", from(clk, chat, alice))

	c.song("2", from(clk, chat, alice))
	c.ready()
	if got := words(c.song("stop", from(clk, chat, alice))); got != lobbyOff {
		t.Errorf("stop: %q", got)
	}
}

// Without buttons, /song start and the lobby's timer at once start the
// game once.
func TestPressRacesTimer(t *testing.T) {
	for range 20 {
		clk, c, _ := signalLobby(t)
		c.react(false, from(clk, chat, alice))
		c.react(false, from(clk, chat, bob))
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); clk.Advance(lobbyWait) }()
		got := words(c.song("start", from(clk, chat, alice)))
		wg.Wait()
		if got != startingNow && got != alreadyBegun {
			t.Errorf("start %q", got)
		}
		if p := c.outbox("0"); songPosts(p.Ops) != 1 {
			t.Fatalf("song posts %+v", p)
		}
		c.close()
	}
}

// /song in the lobby says the game is waiting for players, not on a song,
// in the words of the network: press ✅, or react.
func TestLobbyBusy(t *testing.T) {
	clk, c, _, _, r := buttonLobby(t, false)
	if got := words(c.song("", from(clk, chat, bob))); got != readyBusyText || strings.Contains(got, "react") {
		t.Errorf("in the lobby: %q", got)
	}
	c.press("ready:"+r.Meta.Watch.ID, from(clk, chat, bob))
	c.press("start:"+r.Meta.Watch.ID, from(clk, chat, alice))
	if got := words(c.song("", from(clk, chat, bob))); got != "A song quiz is on: song 1 of 2." {
		t.Errorf("after it: %q", got)
	}
	clk2, c2, _ := signalLobby(t)
	if got := words(c2.song("", from(clk2, chat, bob))); got != lobbyBusyText {
		t.Errorf("without buttons: %q", got)
	}
}

// On a bot on two networks whose Telegram shows buttons, a Signal group's
// lobby is one for reactions: its chat shows none.
func TestNoButtonsChat(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	d := newFakeDeezer(t)
	c := connect(t, testServer(t, t.TempDir(), clk, &syncBuffer{}, d.srv.URL))
	c.initializeButtons()
	ctx := from(clk, chat, alice)
	ctx.Network, ctx.Buttons = "signal", false
	r := c.song("2", ctx)
	if r.Meta == nil || r.Meta.Buttons != nil || r.Meta.OnPress != "" || !r.Meta.Watch.Reactions || !strings.Contains(words(r), "React to this message") {
		t.Fatalf("a Signal lobby: %q, %+v", words(r), r.Meta)
	}
}

// /song games reads the model's reply robustly: a reply of ids alone
// names no games, so the songs are chosen by script; junk, ids without a
// game and repeats are passed over; a game's name must be whole words of
// the title or artist, so a tiny one doesn't match any title.
func TestGamesReplies(t *testing.T) {
	for _, tc := range []struct {
		reply, log string
		want       []int64
	}{
		{`{"english":[<Halo Theme>,<Sweden>,<Minecraft (Main Theme)>,<Song of Storms>]}`, "by script instead: no games named", []int64{301, 302, 303, 304}},
		{`{"english":["x",{"id":<Halo Theme>,"game":"Halo"},{"id":<Minecraft (Main Theme)>},{"id":<Halo Theme>},{"id":<Song of Storms>,"game":""},7,null]}`, "named after their game: 1 of 4", []int64{301}},
		{`{"english":[{"id":<Halo Theme>,"game":"Ha"},{"id":<Sweden>,"game":"S"},{"id":<Minecraft (Main Theme)>,"game":"Mine"}]}`, "named after their game: 0 of 4", nil},
		{`not JSON`, "by script instead: no usable reply", []int64{301, 302, 303, 304}},
	} {
		clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
		logs := &syncBuffer{}
		d := newFakeDeezer(t)
		d.mu.Lock()
		d.charts["pl4380597982"] = gameTracks
		d.mu.Unlock()
		c := connect(t, testServer(t, t.TempDir(), clk, logs, d.srv.URL))
		var tokens int
		c.judge = func(params json.RawMessage) string {
			var p struct {
				MaxTokens int `json:"maxTokens"`
				sampleRequest
			}
			json.Unmarshal(params, &p)
			tokens = p.MaxTokens
			var items []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			}
			json.Unmarshal([]byte(p.Messages[0].Content.Text), &items)
			reply := tc.reply // the songs are shuffled: their ids by title
			for _, it := range items {
				reply = strings.ReplaceAll(reply, "<"+it.Title+">", strconv.Itoa(it.ID))
			}
			return reply
		}
		caps := map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReplies": true}}, "sampling": map[string]any{}}
		c.result("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": caps}, new(initializeResult))
		c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
		dm := from(clk, chat, alice)
		dm.Group = false
		c.song("games 2", dm)
		waitFor(t, c.s, "the songs chosen for "+tc.reply, func() bool {
			g := c.s.games[chat]
			return strings.Contains(logs.String(), tc.log) && (g == nil || g.Post != "")
		})
		c.s.mu.Lock()
		var got []int64
		if g := c.s.games[chat]; g != nil {
			for _, s := range append(slices.Clone(g.Songs), g.Pool...) {
				got = append(got, s.ID)
				if wantGame := tc.log == "named after their game: 1 of 4"; (s.Game != "") != wantGame {
					t.Errorf("%s: %s has game %q", tc.reply, s.Title, s.Game)
				}
			}
		}
		c.s.mu.Unlock()
		if len(got) == 0 != (len(tc.want) == 0) || slices.ContainsFunc(got, func(id int64) bool { return !slices.Contains(tc.want, id) }) { // the first may be dropped, if its clip fails
			t.Errorf("%s: songs %v, want %v", tc.reply, got, tc.want)
		}
		if tokens < judgeTokens {
			t.Errorf("%s: max tokens %d", tc.reply, tokens)
		}
	}
}
