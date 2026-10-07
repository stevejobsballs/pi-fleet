# pi-fleet trial kit

Scripts to put pi-fleet on two Raspberry Pis for a trial: one **master Pi**
and one **employee Pi** (Pi 4 or 5, 64-bit Raspberry Pi OS, same network).
Full background is in [docs/OPERATIONS.md](../../docs/OPERATIONS.md).

The trial differs from a production install: the master uses a
self-signed certificate on port 8443 and keeps its data on the SD card
(no SSD or backup disk). Everything else is the real thing.

Every script accepts `--dry-run`, which prints what it would do without
changing anything. Use it first if you want to see the steps.

**Before you start, give each Pi its own host name.** Both Pis are called
`raspberrypi` out of the box, and then the employee Pi can't find the master
by name. On each Pi:

```sh
sudo raspi-config nonint do_hostname fleet-master     # or e.g. fleet-tess on the employee Pi
sudo reboot
```

If you began installing by hand from the earlier instructions, that's fine:
the scripts keep what is already there and carry on.

## 1. Master Pi

On the master (this can be your development Pi), from the repo:

```sh
cd ~/pi-fleet/deploy/trial
sudo ./master-setup.sh --binary ~/pi-fleet/dist/v0.1.0/pi-fleet_v0.1.0_linux_arm64
```

It asks you to choose the first super user's name and password. Then open
the address it prints, sign in, and on the web interface:

1. **Sites**: create a site and a location.
2. **Users**: create an account for yourself as an ordinary user. Note the
   one-time password it shows.

## 2. Employee Pi

Copy the kit, the release and the master's certificate to the employee Pi
(run on the master; replace `you@employee-pi`):

```sh
scp -r ~/pi-fleet/deploy/trial ~/pi-fleet/dist/v0.1.0/pi-fleet_v0.1.0_linux_arm64 /etc/pi-fleet/tls/cert.pem you@employee-pi:
```

Then on the employee Pi:

```sh
cd ~/trial
sudo ./node-setup.sh --binary ~/pi-fleet_v0.1.0_linux_arm64 --ca ~/cert.pem \
  --master https://<master-name>.local:8443 --username <the user you created>
```

Enter the one-time password and choose your own. The script shows six
words and waits: on the master's web interface open **Pis**, type the words
into the pending Pi's row and press **Confirm**. The script then finishes
and starts the service. Open http://127.0.0.1:8080 on the employee Pi.

## 3. Try it

- On the master: register equipment, create a schedule due within a month,
  open a work order and assign it to your user.
- On the employee Pi (it syncs every 5 minutes): start the work, record a
  calibration or checklist, log time, sign it.
- Unplug the employee Pi's network, do more work, plug it back in, and see
  it reach the master.
- On the master: open the work order's **Audit trail** and **Printable record**.
- `sudo ./check.sh` on either Pi shows its state at any time.

## 4. Test an update

This is the part that most needs a real install. On the development Pi:

```sh
cd ~/pi-fleet
git tag -a v0.2.0 -m "pi-fleet v0.2.0"
make release VERSION=v0.2.0
bin/pi-fleet release-sign -key "/media/clooney/USB STICK/pi-fleet release keys/pi-fleet-release.key" -dir dist/v0.2.0 -version v0.2.0
sudo cp dist/v0.2.0/* /srv/pi-fleet/releases/        # on the master
```

Then on the master, followed by the employee Pi:

```sh
sudo ./update.sh            # on the master: it finds /srv/pi-fleet/releases
```

`update.sh` stops the service, installs the release, starts it again, and
lists any data files not owned by the `pifleet` user. There should be none.
If there are, or anything else goes wrong, send the output.

## Starting over

```sh
sudo systemctl disable --now pi-fleet
sudo rm -rf /opt/pi-fleet /srv/pi-fleet /var/lib/pi-fleet /etc/pi-fleet /etc/systemd/system/pi-fleet.service
sudo userdel pifleet
```
