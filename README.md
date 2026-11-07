# Virtual Controller

Use your phone as a wireless Xbox 360 controller for PC games.

Touch controls on your phone → PC thinks a real Xbox controller is plugged in. Works with Wine, Steam, emulators — anything that supports a gamepad.

![gamepad](./screenshots/layout.jpeg)

## How to use

```bash
go build -o controller-server .
sudo cp 99-uinput.rules /etc/udev/rules.d/ && sudo udevadm control --reload-rules && sudo udevadm trigger
sudo usermod -aG wheel "$USER"   # log out & back in after this
./controller-server
```

Scan the QR code printed in the server terminal with your phone. It opens the controller page with a pairing token already set. Connect, press **▶ Start**, then rotate to landscape.

The token is generated on first run and saved as `AUTH_TOKEN` in `.env`. Everything else on your network is refused without it, so don't share the QR code or the token.

That's it. The virtual controller appears on your PC, games detect it.

## What you can do

- Tap buttons, pull triggers, move sticks — just like a real controller
- Works with multiplayer games — each phone gets its own controller
- Edit mode (✎): drag controls anywhere, resize them, snap to grid
- Adjust deadzone, trigger range, and polling speed in ⚙ settings
- Game running won't let your PC sleep

## APIs

| Endpoint | What it does |
|---|---|
| `/api/config` | Read/change settings (needs `?token=` or `Authorization: Bearer`) |
| `/api/layout` | Save/load control positions (same auth; must be a JSON object) |
| `/api/layout?name=X` | Save/load/delete a named layout profile (`Default` = `layout.json`) |
| `/api/layouts` | List layout profiles |
| `/api/status` | Connected players: slot, IP, connect time |
| `/ws` | Controller input (same auth) |
