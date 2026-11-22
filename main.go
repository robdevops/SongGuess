// Command songs is a song-guessing game for signalbot: /song plays a few
// songs from a Deezer chart, each as a short clip cut at random from its
// 30-second preview, players reply to each clip with its title or artist,
// and when a song's time is up the bot reveals it, with who got it, and
// plays the next.
//
// It is an MCP server on stdio (protocol 2025-06-18, newline-delimited
// JSON-RPC) that speaks signalbot's addon extension: signalbot starts it as
// a child process, calls its tools for /song and for each reply its watch
// takes, and reads its outbox for the songs, reveals and results. It uses
// no model. Stdout carries nothing but the protocol. It keeps its state and
// its own log in SONGS_STATE_DIR; the log never holds a title, an artist,
// an answer, a name or message text.
//
// Every setting is a SONGS_ variable, which signalbot passes to an addon
// server called songs; see README.md.
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/robdevops/chat-plugin-songs/internal/httpx"
)

// maxLog is the size past which the log is moved aside at start, keeping
// one old log.
const maxLog = 4 << 20

func main() { os.Exit(run()) }

func run() int {
	// Stdout is the protocol's: anything else printed by mistake goes to
	// stderr, which signalbot counts but never logs.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	cfg, err := loadConfig(os.Getenv)
	if err == nil {
		err = checkDir(cfg.StateDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "songs: %v\n", err)
		return 1
	}
	debug.SetMemoryLimit(cfg.MemLimit)
	logger, closeLog, err := openLog(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "songs: %v\n", err)
		return 1
	}
	defer closeLog()
	s, err := newServer(cfg, realClock{}, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), httpx.Client(4), logger)
	if err != nil {
		logger.Printf("can't start: %v", err)
		fmt.Fprintf(os.Stderr, "songs: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.srv.Run(ctx, os.Stdin, stdout) }()
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	s.shutdown()
	s.save()
	if err != nil {
		logger.Printf("stopped: %v", err)
		return 1
	}
	logger.Printf("stopped")
	return 0
}

// openLog opens the game's own log, songs.log in dir, moving a large one
// aside to songs.log.1 first.
func openLog(dir string) (*log.Logger, func(), error) {
	path := filepath.Join(dir, "songs.log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("can't open its log in SONGS_STATE_DIR: %v", err)
	}
	return log.New(f, "", log.LstdFlags), func() { f.Close() }, nil
}
