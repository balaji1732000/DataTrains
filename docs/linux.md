# Linux collector

The Trajectory Collector can be built as a native Tauri desktop application on
Ubuntu and other Linux distributions supported by Tauri 2.

## Ubuntu build prerequisites

```bash
sudo apt update
sudo apt install libwebkit2gtk-4.1-dev build-essential curl wget file \
  libxdo-dev libssl-dev libayatana-appindicator3-dev librsvg2-dev \
  libdbus-1-dev pkg-config ffmpeg xinput x11-utils x11-xserver-utils
```

Start the development application:

```bash
npm run collector:desktop:dev
```

Build the installable Ubuntu/Debian package:

```bash
npm run collector:package:linux
```

An AppImage can be built separately on a standard Tauri Linux build host:

```bash
npm run collector:package:linux:appimage
```

## Display-session requirement

V1 records the X11 desktop with FFmpeg and observes task-scoped XInput events.
On Ubuntu's login screen, choose **Ubuntu on Xorg** from the session menu before
signing in. The application itself opens on Wayland, but recording preflight
fails closed because Wayland does not allow ordinary applications to observe
global keyboard and pointer activity. This prevents incomplete trajectories
from being submitted as valid recordings.

Linux stores key codes such as `X11_38`, never translated text. Clipboard
content is not captured. The same allowlist and sensitive-window policy used on
Windows automatically pauses the recorder when focus leaves the task app.
