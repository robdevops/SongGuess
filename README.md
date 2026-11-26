# SongGuess

A song-guessing game for [signalbot](https://github.com/robdevops/signalbot).
`/song` plays a few songs from a Deezer chart, each as a short clip. Players
reply with the title or the artist, and the bot reveals each song with its
album cover. After the last song, it ranks the players and keeps a 7-day
scoreboard.

```
/song                  SONGS_SONGS songs (5 by default) from Deezer's chart of all genres
/song rock             from a genre's chart: Deezer's genre names, without "Music", such as pop, rock, rap, hip hop, latin, games
/song 80s              a decade's hits, from Deezer's editors' playlists: 60s, 70s, 80s, 90s, 2000s, 2010s, 2020s
/song pop 10s 3        with a clip length (1s to 25s) and a number of songs (1 to 10), at the end, in either order
/song help             every command, with the defaults in use
/song genres           list Deezer's genres and the decades; an unknown genre lists some genres too
/song start            in a group's lobby, start now, once 2 have joined (only the player who started it; counts as their join)
/song wait             in a group's lobby, wait 30 seconds longer each time, up to the 15-minute call-off (only the player who started it)
/song end, /song stop  finish now: the open song's reveal and the results, or call off the lobby (only the player who started it)
/song scores           this chat's totals over the last 7 days
```

## How to play

- **Join.** In a group, press ✋ Ready under the lobby message on
  Telegram, Discord and Slack, or react to it with any emoji (on Signal,
  the only way). Each player joins for themselves; the player who starts
  it must join too. The game starts 30 seconds after the last player
  joins, once there are at least 2. It is called off if fewer than 2 have
  joined after 15 minutes. The songs are chosen while players join; if
  that takes longer, the bot says it's getting them ready.
- **Lobby buttons.** Where the bot's network shows buttons (Telegram,
  Discord, Slack), the lobby message has ✋ Ready, for anyone (a pop-up
  says how many have joined, or that you're already in), and under it
  three for the player who started it: ▶️ Start (start now, once 2 have
  joined; the press counts as their join), ✖️ Cancel (call it off) and
  ⏱ +30s (wait 30 seconds longer each press, up to the 15-minute
  call-off). Anyone else pressing those gets a pop-up saying only the
  starter can. On Signal, type `/song start`, `/song stop` and
  `/song wait` instead.
- **DMs.** There is no lobby: the game starts at once. Any message is an
  answer.
- **Answer.** In a group, reply to the song's message. Name the title, the
  artist, or both, such as `Bohemian Rhapsody by Queen`. Your last answer
  before time is up counts.
- **Points.** 10 for the title and 5 for the artist. The first player to
  get either gets 5 more, marked ⚡.
- **Early end.** A song ends as soon as every player has answered.
- **Hurry.** The bot reacts ⏰ to the song with 5 seconds left.
- **No repeats.** A song played in a chat isn't played there again for 30
  days.
- **After the game.** For an hour, a reply to one of its messages gets
  "That song quiz is over. Start another with /song."

By default the songs are sung mainly in English, as the bot's model judges.

## Quick start

You need signalbot at `c57871e` (branch `claude/busy-maxwell-jo5ojy`, or main once it's merged) or newer (for the lobby's buttons; `c755fc7` or newer for the ⏰ reaction to its own post), and Go 1.27 or newer.

1. **Build it:**

   ```
   git clone https://github.com/robdevops/SongGuess.git
   cd SongGuess
   CGO_ENABLED=0 go build -ldflags='-s -w' -o /tmp/songs . && sudo install -D -m 755 /tmp/songs /opt/signalbot-addons/songs
   ```

2. **Add it to the MCP servers file** (`MCP_CONFIG`). It must be called
   `songs`. `chats: ["all"]` plays in every group; list group IDs to
   limit it, and add `"dms": true` to play in DMs too. `react` is for
   the ⏰ on each song with 5 seconds left. Keep the `model`
   permission: the bot's model judges the answers and picks English
   songs. Without it, a stricter built-in match judges.

   ```jsonc
   "songs": {
     "command": "/opt/signalbot-addons/songs",
     "chat_commands": {"song": {"tool": "song", "direct": true, "example": "/song rock", "timeout": "60s"}},
     "addon": {
       "chats": ["all"],
       "post": true, "react": true, "watch": true,
       "model": {"per_hour": 120, "max_tokens": 600}
     }
   }
   ```

3. **Restart signalbot** and type `/song` in the group.

Nothing else needs setting: Deezer needs no key. All the settings (such
as `SONGS_ANY_LANGUAGE`), the permissions, and the Docker and systemd
notes are in [docs/DETAILS.md](docs/DETAILS.md#settings-in-full).

## Credits and privacy

The songs, covers and previews come from Deezer's public API, with thanks
to [Deezer](https://www.deezer.com). Whether posting clips fits Deezer's
API terms is for the bot's operator to check. With the `model` permission,
answers, titles and artists go to the bot's model, without names. The game
never logs names or message text.

More: [docs/DETAILS.md](docs/DETAILS.md)
