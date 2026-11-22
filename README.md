# Songs

A song-guessing game for [signalbot](https://github.com/robdevops/signalbot).
`/song` plays a few songs from a Deezer chart, each as a short clip (5
seconds by default) cut at random from the song's 30-second preview. Players
**reply** to each clip with its title or its artist; when a song's time is up
the bot reveals it, with its album cover and who got it, and plays the next.
After the last, it ranks the players, and keeps a 7-day scoreboard.

```
/song                  SONGS_SONGS songs (5 by default) from Deezer's chart of all genres
/song rock             from a genre's chart: Deezer's genre names, such as pop, rock, rap, hip hop, dance, r&b, jazz
/song 80s              a decade's hits, from Deezer's editors' playlists: 60s, 70s, 80s, 90s, 2000s, 2010s, 2020s
/song pop 10s 3        with a clip length (1s to 25s) and a number of songs (1 to 10), at the end, in either order
/song help             every command, with the defaults in use
/song genres           list Deezer's genres; an unknown genre lists some of them too
/song end              finish now: the open song's reveal and the results (only the player who started it)
/song scores           this chat's totals over the last 7 days
```

It is a separate program that signalbot starts as an
[MCP addon](https://github.com/robdevops/signalbot/blob/main/docs/FILES_LINKS_TOOLS_MCP.md#addons) server. It speaks MCP
2025-06-18 over stdio (newline-delimited JSON-RPC), with signalbot's addon
extension, through signalbot's own Go package for addons,
[`github.com/robdevops/signalbot/addon`](https://github.com/robdevops/signalbot/tree/main/addon),
and no other MCP library. It uses no model and holds no API key: Deezer's
public API needs none.

## How to play

- **Joining.** In a group, `/song` starts with a lobby: react to its intro
  with any emoji to join (take the reaction back to leave). The game starts
  30 seconds after the last player joins, once there are at least 2 of you,
  and is called off if fewer than 2 have joined after 5 minutes; whoever
  starts it has joined. In a DM it starts at once. Those who joined are the
  players every song waits for; anyone may still answer. On Telegram the
  bot sees reactions only as the group's admin; a bot older than signalbot
  fe51891 starts the game at once.

- **Always answer by replying** to the song's message (or any of the game's
  messages: a reply counts for the song that is open). That works on every
  network, and on Telegram whatever the bot's privacy mode.
- **Name the title, the artist, or both** in one reply, such as
  `Bohemian Rhapsody by Queen`. When a song ends, the bot's model judges
  every answer, the title and the artist separately, as a friendly quiz host
  would: misspellings, "wanna" for "want to", word order and the like are
  fine; a different song, a list of guesses or unrelated chat isn't. The
  model needs the addon block's `model` permission (below); without it, or
  if it fails, a stricter built-in match decides: case, accents and
  punctuation don't matter, one or two letters may be wrong, and a long
  reply listing guesses names nothing.
- **Your last answer before time is up counts.** To change it, edit your
  reply, or reply again; a new reply replaces the old one, so give the title
  and the artist in the same one. Answers get no reaction.
- **Points:** 10 for the title, 5 for the artist, and 5 more for the first
  player to get either (marked ⚡).
- **The reveal** replies to the song's message: "✅ **Title** — Artist",
  who got the title and who got the artist, and the album cover. Then a
  5-second pause, and the next song.
- **No waiting once everyone's in.** Song 1 runs its full time (30s by
  default); whoever answers it is playing. From song 2 on, a song ends as
  soon as all of them have answered (3s later, for answers still on their
  way). Someone who joins later still scores, but isn't waited for.
- **No repeats:** a song played in a chat isn't played there again for 30
  days. When a genre's chart has run out, `/song` says so: try another.
- Messages that aren't replies to the game are left alone, so the chat
  carries on around it.

## Install

It needs a signalbot that plays audio in posts and takes reply watches
(`a500bfe` or newer). Get the code and build it with Go 1.27 or newer, as a
static binary, which the Docker image needs (it has no shell or runtimes)
and systemd is happy with:

```
git clone https://github.com/robdevops/chat-plugin-songs.git
cd chat-plugin-songs
CGO_ENABLED=0 go build -ldflags='-s -w' -o /opt/signalbot-addons/songs .
```

The build downloads signalbot's module, at the commit `go.mod` names, for
its `addon` package; only that package, which needs nothing beyond Go's
standard library, is built into the game. The clips are cut in plain Go,
at MP3 frame edges, with no ffmpeg and no re-encoding.

Give its full path as `command`. Under systemd there's nothing to mount:
the unit's `ProtectSystem=strict` leaves `/opt` readable. Under Docker,
mount the directory read-only at the same path, by adding
`- /opt/signalbot-addons:/opt/signalbot-addons:ro` to the volumes in
`compose.yaml`. Then add it to the MCP servers file (`MCP_CONFIG`). The
server must be called `songs`, which is what gives it the `SONGS_` settings
below:

```jsonc
"songs": {
  "command": "/opt/signalbot-addons/songs",
  "chat_commands": {"song": {"tool": "song", "direct": true, "example": "/song rock", "timeout": "60s"}},
  "addon": {
    "chats": ["<group id>"],
    "post": true, "watch": true,
    "model": {"per_hour": 120, "max_tokens": 600}
  }
}
```

`chats` takes the groups' IDs as Signal or Telegram shows them, or `["all"]`;
add `"dms": true` to play in DMs too. The game needs:

- `post`, for each song (a post with the clip as audio: on Telegram a voice
  message, on Signal an attachment), its reveal (a reply to the song, with
  the cover) and the results. The bot drops a post over the chat's post
  `rate` for good, so the game asks for no reactions: two posts per song fit
  the default rate;
- `watch`, to read the replies. A game is at most 10 songs of 2 minutes, so
  the default `watch_max` of 1h is enough.

`"timeout": "60s"` gives `/song` time to fetch the chart and the first
song's preview and cut it. The game gives up a few seconds before the bot's
timeout and says so. The other songs are fetched in the background while
the game plays, each before it is due; a track whose preview can't be
fetched or cut is skipped for another from the chart, and if none can be,
the game ends with the songs it has played.

The game keeps its own files in `SONGS_STATE_DIR`. There's nothing to set
up: signalbot sets it to `addons/songs` in its own state directory
(`/var/lib/signalbot/addons/songs` under systemd, `/data/addons/songs` in
Docker), and the game creates it, for the bot's user alone. To keep them
elsewhere, set `SONGS_STATE_DIR` in `/etc/signalbot.env` to a directory the
bot's user may create or write.

It shares the bot's memory limit, so allow for `SONGS_MEMLIMIT` in
`mem_limit` or `MemoryMax`. The network it needs is `api.deezer.com` and
Deezer's CDN for the previews and covers (`cdnt-preview.dzcdn.net (previews) and cdn-images.dzcdn.net (covers)`), over HTTPS.

## Settings

signalbot passes an addon server called `songs` every `SONGS_` variable from
its environment file, plus `PATH`, `HOME` and the bot's proxy and time zone
settings, and nothing else.

| Variable | Default | Description |
|---|---|---|
| `SONGS_STATE_DIR` | signalbot's `addons/songs` | Where the game keeps `songs.json` and its log. Created, mode 700, if missing; the game exits at start if it can't create, list and write it |
| `SONGS_SONGS` | `5` | A game's songs when `/song` doesn't say, 1 to 10 |
| `SONGS_TIME` | `30s` | The time to answer each song, 10s to 2m |
| `SONGS_MEMLIMIT` | `64MiB` | Memory limit, applied with `debug.SetMemoryLimit`, written as `GOMEMLIMIT` is (`B`, `KiB`, `MiB`, `GiB`, `TiB`) |
| `SONGS_DEEZER` | `https://api.deezer.com` | Deezer's API, for testing against another server |

## Previews from Deezer

The songs, their titles, artists, covers and 30-second previews come from
Deezer's public API (`/genre`, `/chart/{genre}/tracks` and, for decades,
the playlists of Deezer's editors, which need no key), with thanks to
[Deezer](https://www.deezer.com) for them. The previews are
Deezer's: the game downloads each one when it is played, cuts a clip from
it, and posts the clip to the chat, keeping nothing of it once posted.
Whether posting clips of Deezer's previews in a chat fits
Deezer's API terms is for the
bot's operator to check: running this addon is choosing to attach them.

## Privacy

The game gets only what signalbot's addon block allows: the text after
`/song`, and during a game the replies to its own messages, each with the
chat and sender as pseudonyms and the time the bot got it. It never sees a
phone number, account ID, group ID or, unless `names` is set, a display
name; it writes players as `{u_…}` and the bot puts their names in.
Replies are claimed by the bot, so they are never answered, sent to the
model or kept in the chat's history. With the `model` permission, each
song's answers (what players replied, without their names or pseudonyms),
its title and its artist go to the bot's model to be judged, charged to
the chat's daily budget, one request a song.

Deezer sees the game's requests: the genre asked for, from the bot's
address, and nothing about the chat or its players.

### What it keeps

`$SONGS_STATE_DIR/songs.json`, rewritten whole on each change:

- open games: the chat's pseudonym, who started it, the genre, the tracks
  chosen (Deezer's ID, title, artist and the preview's and cover's URLs),
  the open song's deadline, and for each player (a per-chat pseudonym) only
  their answer to the open song (its text, for the model to judge, until
  the song ends), whether it named the title and the artist and when they
  sent it, and their points so far; and who is playing;
- 7 days of points per chat, by player pseudonym;
- 30 days of the tracks played in each chat, by Deezer's ID, to avoid
  repeats;
- outbox ops the bot hasn't read yet, with their clips and covers, for at
  most an hour.

The clips and covers made ready for the next song are kept only in memory.

`$SONGS_STATE_DIR/songs.log` (moved to `songs.log.1` past 4 MiB at start)
records games started and ended, the number of songs, clip lengths, songs
skipped and why (such as "a preview: status 404"), and how many players
scored. It never holds a title, an artist, a genre, an answer, a name, a
pseudonym, a URL or message text.

## The protocol

The `addon` package speaks the protocol: the handshake, tool calls and
their cancellation, and the outbox the bot reads. The game adds:

- `song` (`{args}`, a direct command) and `song_answer` (`{text}`, the
  watch's tool), both for the bot alone, so neither is offered to the model.
- `/song` checks that the bot offered `watchReplies` in the extension's
  capability, fetches the chart and makes the first song ready before the
  call's `deadline`, less 3s to reply.
- It answers with the intro and one watch for the whole game:
  `replies: true`, no pattern, `tool: "song_answer"`, until 10s past the
  game's end (its songs' time, the pauses, and 30s to spare for fetching).
- Each song is a `post` of its own, with `audio`
  (`{"data": <base64 MP3>, "mimeType": "audio/mpeg"}`, the clip without ID3
  tags, which would name it); song 1 is queued during the `/song` call, so
  it follows the intro. When a song's time is up (3s after its deadline),
  the game queues its reveal as a post with `replyTo` the song's post and
  the cover in `images`, then, 5 seconds later, the next song. After the
  last, the results follow as a new post, then an `unwatch`. `/song end`
  answers with the open song's reveal and the results itself and queues
  only the `unwatch`.
- Ops the bot hasn't read are saved in `songs.json`, with their IDs, so they
  survive a restart. On start, games still open get their timers back, a
  song whose time passed while the game was down is revealed at once
  (without its cover), and the next song is fetched again.
