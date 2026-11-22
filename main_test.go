package main

import (
	"bufio"
	"bytes"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
}

func newFakeDeezer(t *testing.T) *fakeDeezer {
	d := &fakeDeezer{
		charts:  map[string][]track{"0": songs, "152": rockSongs, "116": songs[:2], "pl867825522": rockSongs, "pl868": songs[:3], "pl14917741483": rockSongs},
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
			io.WriteString(w, `{"data":[{"id":0,"name":"All"},{"id":132,"name":"Pop"},{"id":152,"name":"Rock"},{"id":116,"name":"Rap/Hip Hop"},{"id":165,"name":"R&B"}]}`)
		case path == "/search/playlist":
			// A fan's list first, then the editors' "80s Hits", whose ID the
			// test charts hold as "pl" + ID.
			io.WriteString(w, `{"data":[{"id":5,"title":"80s Hits","user":{"name":"Alice"}},{"id":867,"title":"80s Party Hits","user":{"name":"Laeti - Deezer Pop Editor"}},{"id":868,"title":"80s Hits","user":{"name":"Laeti - Deezer Pop Editor"}}]}`)
		case strings.HasPrefix(path, "/chart/") && strings.HasSuffix(path, "/tracks"),
			strings.HasPrefix(path, "/playlist/") && strings.HasSuffix(path, "/tracks"):
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("chart %s", r.URL)
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
	vars := map[string]string{"SONGS_STATE_DIR": dir, "SONGS_DEEZER": api}
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
	caps := map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "network": "signal", "watchReplies": replies}}}
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

// ready waits until no song is being fetched in the background.
func (c *client) ready() {
	c.t.Helper()
	for range 500 {
		c.s.mu.Lock()
		n := len(c.s.preparing)
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
	if !slices.Equal(names, []string{"song", "song_answer"}) {
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

	clk.Advance(25*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 {
		t.Fatalf("song 1's reveal %+v", page)
	}
	want := "✅ **" + song1.Title + "** — " + song1.Artist + "\nTitle: {u_alice} ⚡\nArtist: {u_bob}\nPreviews from Deezer"
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
	want = "✅ **" + song2.Title + "** — " + song2.Artist + "\nTitle: {u_alice} ⚡, {u_bob}\nArtist: {u_alice}, {u_bob}\nPreviews from Deezer"
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
	if len(page.Ops) != 3 || !strings.Contains(page.Ops[0].Text, "\nNobody got it.\nPreviews from Deezer") || page.Ops[0].ReplyTo != s3.ID ||
		page.Ops[1].Text != results || page.Ops[1].ReplyTo != "" {
		t.Fatalf("results %+v\nwant %q", page, results)
	}
	if o := page.Ops[2]; o.Op != "unwatch" || o.Chat != chat || o.Watch != w.ID {
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
	if got := words(c.song("end", from(clk, chat, alice))); got != noGame {
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
	if !strings.HasPrefix(ended, "✅ **"+song.Title+"** — ") || !strings.HasSuffix(ended, "Title: {u_bob} ⚡\nPreviews from Deezer\n\n🏁 **Song quiz over**\n🥇 {u_bob} 15 · 1/1 songs") {
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
	if len(page.Ops) != 4 || page.Ops[0].Audio == nil || !strings.HasPrefix(page.Ops[1].Text, "✅ **Back in Black** — AC/DC\nNobody got it.") ||
		len(page.Ops[1].Images) != 0 || page.Ops[2].Text != "🏁 **Song quiz over.** Nobody scored." || page.Ops[3].Op != "unwatch" {
		t.Fatalf("ops %+v", page)
	}
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
	if o := page.Ops[1]; o.ReplyTo != page.Ops[0].ID || !strings.HasSuffix(o.Text, "\nTitle: {u_alice} ⚡\nArtist: {u_bob}\nPreviews from Deezer") || len(o.Images) != 0 {
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
	if len(page.Ops) != 2 || page.Ops[0].ReplyTo != s2.ID || len(page.Ops[0].Images) != 1 {
		t.Fatalf("song 2's reveal %+v", page)
	}
	checkSong(t, page.Ops[1], 3, 3)
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
	if len(page.Ops) != 1 || page.Ops[0].ReplyTo == "" {
		t.Fatalf("the reveal alone %+v", page)
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
	text := c.outbox("0").Ops[1].Text
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
		"30s":       badClip,
		"0s":        badClip,
		"rock 2.5s": badClip,
		"polka":     "I don't know that genre. Try one of: pop, rock, rap/hip hop, r&b, or /song genres for them all.",
		"genres":    "🎵 **Genres:** pop, rock, rap/hip hop, r&b.\n**Decades:** 1960s, 1970s, 1980s, 1990s, 2000s, 2010s (or 80's for short).\nPlay one with, say, /song rock or /song 1980s; plain /song plays the main chart.",
		"GENRE":     "🎵 **Genres:** pop, rock, rap/hip hop, r&b.\n**Decades:** 1960s, 1970s, 1980s, 1990s, 2000s, 2010s (or 80's for short).\nPlay one with, say, /song rock or /song 1980s; plain /song plays the main chart.",
		"help":      c.s.helpText(),
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
		if gt, ga := matches(tc.reply, titleNames(tc.t)), matches(tc.reply, artistNames(tc.t)); gt != tc.title || ga != tc.artist {
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
	if err != nil || c.Songs != 5 || c.Time != 30*time.Second || c.MemLimit != 64<<20 || c.Deezer != "https://api.deezer.com" || c.gap != gapTime {
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
	clk, c, _, _ := begin(t, "SONGS_TIME", "45s", "SONGS_SONGS", "3")
	got := words(c.song("help", from(clk, chat, alice)))
	for _, want := range []string{"/song — 3 songs from the main chart, 5s clips", "/song rock —", "/song rock 10s 3 —", "/song genres —", "/song 1980s or /song 80's —", "/song end —", "/song scores —", "45s to answer", "10 points"} {
		if !strings.Contains(got, want) {
			t.Errorf("help lacks %q:\n%s", want, got)
		}
	}
}

// /song 80's plays the editors' 80s Hits playlist; bare 80s is 80-second
// clips, too long, with a hint; it
// combines with a clip length; with the pinned playlist gone, the search
// finds the editors' own "80s Hits"; an unknown decade is no genre.
func TestDecade(t *testing.T) {
	clk, c, d, _ := begin(t)
	got := words(c.song("80's 10s 2", from(clk, chat, alice)))
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
	if got := words(c.song("80s", from(clk, "c_five", alice))); got != badClip+" For the decade, write /song 1980s or /song 80's." {
		t.Errorf("/song 80s: %q", got)
	}
	for name, want := range map[string]string{"80's": "80s", "'80s": "80s", "80’s": "80s", "1980s": "80s", "1980's": "80s", "eighties": "80s",
		"2000s": "2000s", "00's": "2000s", "2010s": "2010s", "10's": "2010s", "1960s": "60s", "70's": "70s"} {
		if dec, ok := findDecade(name); !ok || dec.Label != want {
			t.Errorf("findDecade(%q) = %v %v", name, dec, ok)
		}
	}
	// 10s alone is 10-second clips of the main chart; the 2010s are 2010s.
	if got := words(c.song("10s", from(clk, "c_three", alice))); !strings.HasPrefix(got, "🎵 **Song quiz** · ") || !strings.Contains(got, ", 10s clips") {
		t.Errorf("/song 10s: %q", got)
	}
	if got := words(c.song("2010s 10s", from(clk, "c_four", alice))); !strings.HasPrefix(got, "🎵 **Song quiz: 2010s** · ") || !strings.Contains(got, ", 10s clips") {
		t.Errorf("/song 2010s 10s: %q", got)
	}
	for _, name := range []string{"80s", "60s", "00s", "50's", "1950s", "1985s", "80", "20s", "rock", "8's"} {
		if _, ok := findDecade(name); ok {
			t.Errorf("findDecade(%q) passed", name)
		}
	}
}
