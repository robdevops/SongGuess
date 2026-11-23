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
/song end, /song stop  finish now: the open song's reveal and the results (only the player who started it)
/song scores           this chat's totals over the last 7 days
```

## How to play

- **Join.** In a group, react to the lobby message with any emoji. The
  player who starts it must react too. The game starts 30 seconds after
  the last player joins, once there are at least 2. It is called off if
  fewer than 2 have joined after 5 minutes.
- **DMs.** There is no lobby: the game starts at once. Any message is an
  answer.
- **Answer.** In a group, reply to the song's message. Name the title, the
  artist, or both, such as `Bohemian Rhapsody by Queen`. Your last answer
  before time is up counts.
- **Points.** 10 for the title and 5 for the artist. The first player to
  get either gets 5 more, marked ⚡.
- **Early end.** A song ends as soon as every player has answered.
- **No repeats.** A song played in a chat isn't played there again for 30
  days.
- **After the game.** For an hour, a reply to one of its messages gets
  "That song quiz is over. Start another with /song."

By default the songs are sung mainly in English, as the bot's model judges.

## Quick start

You need signalbot main at `d685491` or newer, and Go 1.27 or newer.

1. **Build it:**

   ```
   git clone https://github.com/robdevops/SongGuess.git
   cd SongGuess
   CGO_ENABLED=0 go build -ldflags='-s -w' -o /tmp/songs . && sudo install -D -m 755 /tmp/songs /opt/signalbot-addons/songs
   ```

2. **Add it to the MCP servers file** (`MCP_CONFIG`). It must be called
   `songs`. `chats: ["all"]` plays in every group; list group IDs to
   limit it, and add `"dms": true` to play in DMs too. Keep the `model`
   permission: the bot's model judges the answers and picks English
   songs. Without it, a stricter built-in match judges.

   ```jsonc
   "songs": {
     "command": "/opt/signalbot-addons/songs",
     "chat_commands": {"song": {"tool": "song", "direct": true, "example": "/song rock", "timeout": "60s"}},
     "addon": {
       "chats": ["all"],
       "post": true, "watch": true,
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
