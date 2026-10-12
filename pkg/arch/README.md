# Arch Linux package (onedriver-fork)

`PKGBUILD` builds the fork (onedriver, launcher, tray, Qt activity popup) from
the local git checkout. It `provides`/`conflicts` with `onedriver`.

Only committed changes are packaged (makepkg clones the repo), so commit first.

## Build and install

    cd pkg/arch
    makepkg -si

To build from GitHub instead, change the `source=` line in the PKGBUILD to
`git+https://github.com/leifrossau/onedriver.git#branch=fork-improvements`.

## Upgrade

    git pull
    cd pkg/arch && makepkg -si

## Switching from upstream `onedriver`

pacman will ask to remove `onedriver` because of the conflict; answer yes. The
systemd unit name (`onedriver@.service`) and binary paths are unchanged, so an
existing `onedriver@home-you-OneDrive.service` keeps working. Restart it
afterwards to run the new binary:

    systemctl --user restart onedriver@home-you-OneDrive.service

## Enable a mount

    systemctl --user enable --now onedriver@$(systemd-escape --path ~/OneDrive).service

The tray starts automatically at next login (`/etc/xdg/autostart`).
