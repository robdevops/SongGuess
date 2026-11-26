# SongGuess: details

The fine print behind the [README](../README.md): how the game judges
answers, how it is built and run, what it keeps, and the protocol.

## How it runs

It is a separate program that signalbot starts as an
[MCP addon](https://github.com/robdevops/signalbot/blob/main/docs/FILES_LINKS_TOOLS_MCP.md#addons) server. It speaks MCP
2025-06-18 over stdio (newline-delimited JSON-RPC), with signalbot's addon
extension, through signalbot's own Go package for addons,
[`github.com/robdevops/signalbot/addon`](https://github.com/robdevops/signalbot/tree/main/addon),
and no other MCP library. It uses the bot's model, through MCP sampling and
with the `model` permission, to judge answers and to keep English
music, and holds no API key: Deezer's public API needs none.

## Playing, in full

- **Clips.** Each song is a short clip, 5 seconds by default, cut at
  random from the song's 30-second preview.
- **Games:** `/song games` plays video game music, from the playlists of
  Deezer's soundtracks editor (Adventure Mode, Retro Gaming, Minecraft
  Classics, League of Legends), as Deezer's Films/Games chart is mostly film
  songs; with the `model` permission, the model also keeps only game music,
  instrumental or sung mainly in English, even with `SONGS_ANY_LANGUAGE`.
- **Songs in English only**, unless `SONGS_ANY_LANGUAGE` is `true`. Deezer
  doesn't say a song's language, so when a game starts the bot's model
  picks the English ones from the chart; without the `model` permission,
  only songs titled in another script (such as Korean or Japanese) are
  left out.
- **Joining.** A press of ✅ and a reaction are the same join, each
  player's own, saved alike. Taking a reaction back leaves the lobby,
  however you joined. Where the lobby has buttons, each join or leave
  replaces its row (a `buttons` op), with the new count in ✅ and
  ▶️ from 2 players. Those who
  joined are the players every song waits for; anyone may still answer.
  On Telegram the bot sees reactions only as the group's admin.
- **Answering.** In a group, a reply to any of the game's messages counts
  for the song that is open. That works on every network, and on Telegram
  whatever the bot's privacy mode.
- **Judging.** When a song ends, the bot's model judges every answer, the
  title and the artist separately, as a friendly quiz host would:
  misspellings, "wanna" for "want to", word order and the like are fine; a
  different song, a list of guesses or unrelated chat isn't. The model
  needs the addon block's `model` permission; without it, or if it fails, a
  stricter built-in match decides: case, accents and punctuation don't
  matter, one or two letters may be wrong, and a long reply listing guesses
  names nothing.
- **Your last answer before time is up counts.** To change it, edit your
  reply, or reply again; a new reply replaces the old one, so give the title
  and the artist in the same one. Answers get no reaction.
- **The reveal** replies to the song's message: "✅ **Title** — Artist",
  who got the title and who got the artist, and the album cover. Then a
  10-second pause, and the next song.
- **No waiting once everyone's in.** A song ends as soon as every player
  has answered (3s later, for answers still on their way): in a DM, as soon
  as you answer. With no lobby in a group (a bot that can't pass
  reactions), song 1 runs its full time (35s by default) and whoever
  answers it is playing from song 2 on. Someone who answers without having
  joined still scores, but isn't waited for.
- **No repeats:** when a genre's chart has run out, `/song` says so: try
  another.
- In a group, messages that aren't replies to the game are left alone, so
  the chat carries on around it. The bot keeps the hour of "over" replies
  in memory only, so a restart forgets it.

## Building and running

Build with Go 1.27 or newer, as a static binary, which the Docker image
needs (it has no shell or runtimes) and systemd is happy with.

The build downloads signalbot's module, at the commit `go.mod` names, for
its `addon` package; only that package, which needs nothing beyond Go's
standard library, is built into the game. The clips are cut in plain Go,
at MP3 frame edges, with no ffmpeg and no re-encoding.

Give its full path as `command`. Under systemd there's nothing to mount:
the unit's `ProtectSystem=strict` leaves `/opt` readable. Under Docker,
mount the directory read-only at the same path, by adding
`- /opt/signalbot-addons:/opt/signalbot-addons:ro` to the volumes in
`compose.yaml`. Then add it to the MCP servers file (`MCP_CONFIG`). The
server must be called `songs`, which is what gives it the `SONGS_` settings.

`chats` takes the groups' IDs as Signal or Telegram shows them, or `["all"]`;
add `"dms": true` to play in DMs too. The game needs:

- `post`, for each song (a post with the clip as audio: on Telegram a voice
  message, on Signal an attachment), its reveal (a reply to the song, with
  the cover) and the results. The bot drops a post over the chat's post
  `rate` for good, so answers get no reaction;
- `react`, for the ⏰ the game puts on each song's post with 5 seconds
  left. It counts against the same per-chat `rate` (by default 20 a minute,
  burst 5): two posts and one reaction per song fit the default rate.
  Without `react`, the bot fails the ⏰ op and the game plays on without it;
- `watch`, to read the replies, and the reactions that join the lobby. A
  game is at most 10 songs of 2 minutes and a 15-minute lobby, so the
  default `watch_max` of 1h is enough.

### Timeouts and fetching

`"timeout": "60s"` gives `/song` time to fetch the chart and the first
song's preview and cut it, in a DM or a group without a lobby. The game
gives up a few seconds before the bot's timeout and says so. With a lobby,
`/song` answers at once, and the chart, the model's pick of English songs
and the first song's clip are made ready in the background while players
join, for at most 2 minutes; if they fail, the game posts why and is
off. The other songs are fetched in the background while the game plays,
each before it is due; a track whose preview can't be fetched or cut is
skipped for another from the chart, and if none can be, the game ends with
the songs it has played.

### State directory, memory and network

The game keeps its own files in `SONGS_STATE_DIR`. There's nothing to set
up: signalbot sets it to `addons/songs` in its own state directory
(`/var/lib/signalbot/addons/songs` under systemd, `/data/addons/songs` in
Docker), and the game creates it, for the bot's user alone. To keep them
elsewhere, set `SONGS_STATE_DIR` in `/etc/signalbot.env` to a directory the
bot's user may create or write.

It shares the bot's memory limit, so allow for `SONGS_MEMLIMIT` in
`mem_limit` or `MemoryMax`. The network it needs is `api.deezer.com` and
Deezer's CDN for the previews (`cdnt-preview.dzcdn.net`) and covers
(`cdn-images.dzcdn.net`), over HTTPS.

## Settings, in full

signalbot passes an addon server called `songs` every `SONGS_` variable from
its environment file, plus `PATH`, `HOME` and the bot's proxy and time zone
settings, and nothing else.

| Variable | Default | Description |
|---|---|---|
| `SONGS_STATE_DIR` | signalbot's `addons/songs` | Where the game keeps `songs.json` and its log. Created, mode 700, if missing; the game exits at start if it can't create, list and write it |
| `SONGS_SONGS` | `5` | A game's songs when `/song` doesn't say, 1 to 10 |
| `SONGS_TIME` | `35s` | The time to answer each song, 10s to 2m |
| `SONGS_MEMLIMIT` | `64MiB` | Memory limit, applied with `debug.SetMemoryLimit`, written as `GOMEMLIMIT` is (`B`, `KiB`, `MiB`, `GiB`, `TiB`); at least 16MiB |
| `SONGS_DEEZER` | `https://api.deezer.com` | Deezer's API, for testing against another server |
| `SONGS_ANY_LANGUAGE` | `false` | `true` plays songs in any language, except in `/song games`; otherwise only songs sung mainly in English, as the bot's model judges them when a game starts (one request), or, without the model, songs whose title and artist are in Latin script |

## Previews from Deezer

The songs, their titles, artists, covers and 30-second previews come from
Deezer's public API (`/genre`, `/chart/{genre}/tracks` and, for decades
and games, the playlists of Deezer's editors, which need no key), with thanks to
[Deezer](https://www.deezer.com) for them. The previews are
Deezer's: the game downloads each one when it is played, cuts a clip from
it, and posts the clip to the chat, keeping nothing of it once posted.
Whether posting clips of Deezer's previews in a chat fits
Deezer's API terms is for the
bot's operator to check: running this addon is choosing to attach them.

## Privacy

The game gets only what signalbot's addon block allows: the text after
`/song`, and during a game the replies to its own messages (in a DM, every
message) and, in the lobby, the reactions to them, each with the chat and
sender as pseudonyms and the time the bot got it. It never sees a
phone number, account ID, group ID or, unless `names` is set, a display
name; it writes players as `{u_…}` and the bot puts their names in.
Replies are claimed by the bot, so they are never answered, sent to the
model or kept in the chat's history. With the `model` permission, each
song's answers (what players replied, without their names or pseudonyms),
its title and its artist go to the bot's model to be judged, one request a
song; and when a game starts, the titles and artists of the chart's
tracks, to keep English music, one request. Both are charged to
the chat's daily budget.

Deezer sees the game's requests: the genre asked for, from the bot's
address, and nothing about the chat or its players.

### What it keeps

`$SONGS_STATE_DIR/songs.json`, rewritten whole on each change:

- open games: the chat's pseudonym, who started it, the genre (or decade)
  and Deezer's ID for it, whether its songs are still being chosen, in the lobby
  who has joined and when, the tracks chosen (Deezer's ID, title, artist
  and the preview's and cover's URLs), the open song's deadline, and for
  each player (a per-chat pseudonym) only their answer to the open song
  (its text, for the model to judge, until the song ends), whether it named
  the title and the artist and when they sent it, and their points so far;
  and who is playing;
- 7 days of points per chat, by player pseudonym;
- 30 days of the tracks played in each chat, by Deezer's ID, to avoid
  repeats;
- outbox ops the bot hasn't read yet, with their clips and covers, for at
  most an hour.

The clips and covers made ready for the next song are kept only in memory.

`$SONGS_STATE_DIR/songs.log` (moved to `songs.log.1` past 4 MiB at start)
records games started and ended, the number of songs, clip lengths, songs
skipped and why (such as "a preview: status 404"), how many joined a
lobby, how many songs the model kept as English, and how many players
scored. It never holds a title, an artist, a genre, an answer, a name, a
pseudonym, a URL or message text.

## The protocol

The `addon` package speaks the protocol: the handshake, tool calls and
their cancellation, and the outbox the bot reads. The game adds:

- `song` (`{args}`, a direct command) and `song_answer` (`{text}`, or
  `{reaction, removed}` for a lobby's reactions; the watch's tool), both
  for the bot alone, so neither is offered to the model.
- `/song` checks that the bot offered `watchReplies` in the extension's
  capability and finds the genre (Deezer's genre list, kept for a day).
  Without a lobby (a DM, or a bot that doesn't offer `watchReactions`), it
  then fetches the chart and makes the first song ready before the call's
  `deadline`, less 3s to reply. With a lobby, it answers with the intro at
  once and does that in the background, for at most 2 minutes, sampling
  the model outside the call, charged to the chat. If the lobby is ready
  first, it posts "🎵 Getting the songs ready…" and posts song 1 once they
  are; if they fail, it posts what `/song` would have answered (such as
  Deezer being unreachable or the chart having run out), then an
  `unwatch`. A lobby called off stops the choosing, and nothing more is
  posted.
- It answers with the intro and one watch for the whole game:
  `replies: true`, `reactions: true` with a lobby, no pattern,
  `tool: "song_answer"`, `over` (the "That song quiz is over" reply), until
  10s past the game's end (its songs' time, the pauses, 30s to spare for
  fetching, and 15 minutes for a lobby, during which the songs are chosen,
  or the 2 minutes for choosing them, if longer).
- Each song is a `post` of its own, with `audio`
  (`{"data": <base64 MP3>, "mimeType": "audio/mpeg"}`, the clip without ID3
  tags, which would name it); without a lobby, song 1 is queued during the
  `/song` call, so it follows the intro, and with one, when the lobby
  starts the game. When a song's time is up (3s after its deadline),
  the game queues its reveal as a post with `replyTo` the song's post and
  the cover in `images`, then, 10 seconds later, the next song. After the
  last, the results follow as a new post, then an `unwatch`. `/song end`
  (or `stop`) answers with the open song's reveal and the results itself
  and queues only the `unwatch`; in the lobby it calls the game off. A
  lobby called off for want of players (or, with buttons, unstarted after
  15 minutes) posts why, then an `unwatch`.
- Where the bot offers `buttons`, the lobby's answer has one row,
  `onPress: "song_press"`: `✅ N` (Ready, `ready:<id>`, anyone's),
  `Songs: N` (`t1:<id>`), `▶️` (Start, `start:<id>`, shown from 2
  players) and `✖️` (Cancel, `cancel:<id>`), `<id>` the watch's; an
  older lobby's `Clip` toggle (`t2:<id>`) answers "That button is gone."
  and changes nothing. The lobby then has no countdown: only ▶️ or
  `/song start` starts it, and `/song wait` (or an older lobby's
  `wait:<id>`) says there's none. The `Songs` toggle switches between the
  number the lobby opened with and double it, capped at 10 (one mode if
  already there); a toggle saves the game and, if the songs already
  chosen were chosen for fewer, or are
  still being chosen, chooses them again in the background. Every change
  of the players or a toggle queues
  `{"op": "buttons", "chat": <chat>, "watch": <id>, "buttons": [[…]]}`
  with the new row (signalbot `f6a3c5c` or newer; elsewhere a no-op); a
  toggle's op also carries `"text"`, the intro rewritten for the new
  number of songs, which replaces the lobby message's text (signalbot
  `2173af6` or newer). The
  watch runs long enough for double the songs, chosen again at the
  lobby's last moment.
- 5 seconds before a song's deadline, the game queues
  `{"op": "react", "chat": <chat>, "post": <the song's post's op ID>, "emoji": "⏰"}`:
  a reaction to its own post, by the op ID, in place of a context's `ref`.
  On Telegram, which takes only some emoji as reactions, the emoji is 😱:
  the game keeps the chat's network from when it started (the context's
  `network` when the bot is on several, else the bot's).
  It needs the `react` permission and signalbot `c755fc7` (branch
  `claude/mcp-addons-geoguess`, or main once it's merged) or newer; an
  unknown post fails the op, with no retry. There's no ⏰ on a song that
  ended early (everyone answered: its timer is stopped), after
  `/song end`, on a song shorter than 10s (the game never posts one, as a
  song's time is at least 10s), on a song with 5s or less left when the
  game comes back from a restart (with more left, it is armed again), or
  on a song whose post the outbox refused (it has no op ID). If the bot
  dropped the song's post and sends it again later, the ⏰ can come first:
  the bot fails it as an unknown post, and the song has no ⏰. Every song is
  a post the game queued, so each has one: none is the `/song` call's
  answer.
- Ops the bot hasn't read are saved in `songs.json`, with their IDs, so they
  survive a restart. On start, games still open, and lobbies, get their
  timers back, a song whose time passed while the game was down is
  revealed at once (without its cover), and the next song is fetched
  again. A game that was choosing its songs keeps its lobby (or its
  players, if the lobby was over) and chooses them again from the start,
  once the bot has redone its handshake, with 2 minutes more.
