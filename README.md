# Songs

A song-guessing game for [signalbot](https://github.com/robdevops/signalbot),
being built: `/song` will play a random 5 seconds of a song's 30-second
preview from Deezer in the chat, and players reply with its title or artist.

So far this holds the clip cutter (`clip.go`). It cuts MP3s at frame edges
in plain Go, with no MP3 library and no re-encoding, and removes the ID3
tags that name the song.
