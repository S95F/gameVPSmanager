gvm quickstart

Standing up a server, from nothing to players. Steps marked "(not in this
build)" are wired but refuse for now; the rest work today.

1. Install gvm on the box (once):
     sudo gvm host init
     sudo gvm doctor

2. Pick a game and read what it needs:
     gvm game show valheim
   It prints the ports, the variables and a ready-to-paste create line.

3. Define the instance (writes a file; changes nothing on the host yet):
     sudo gvm create valheim-1 --game valheim --var public_name="Friends"

4. Put it on the host — accounts, directories, the systemd unit:
     sudo gvm apply valheim-1
   `sudo gvm apply valheim-1 --diff` shows the plan first and changes nothing.

5. Install the game's files:
     sudo gvm install valheim-1        (not in this build)

6. Set any secrets the game needs (gvm game show lists them):
     sudo gvm secret set valheim-1 …   (not in this build)

7. Start it, and watch it reach ready:
     sudo gvm start valheim-1

8. Connect from your game client, on the ports `gvm game show` listed. Opening
   those ports in your firewall is yours to do — see `gvm help firewall`.

`gvm doctor` is the first thing to run when something looks wrong:
`gvm help troubleshooting`.
