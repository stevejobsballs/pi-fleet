# Connecting employee Pis at other sites: a guide for IT

This is for the IT department of an institution that runs pi-fleet, the
equipment maintenance system on Raspberry Pis. One **master Pi** keeps the
institution's records. **Employee Pis** (one per person) and **kiosk Pis**
(shared, in a workshop) work offline and sync with the master Pi. When
every Pi is on one local network nothing needs setting up. This guide covers
Pis at other buildings or sites.

**In one sentence:** give the master Pi a fixed address and a DNS name,
allow HTTPS from each site's Pis to that one address, and don't intercept
that traffic.

## How the Pis communicate

```
 Site B                              Site A (master Pi's site)
 ┌──────────────┐   HTTPS, TCP 443   ┌───────────────────────────┐
 │ Employee Pi  │ ─────────────────▶ │ Master Pi                 │
 │ (outbound    │                    │ pi-fleet.example.org      │
 │  only)       │                    │ records on an external SSD│
 └──────────────┘                    └───────────────────────────┘
 ┌──────────────┐                                 ▲
 │ Kiosk Pi     │ ────────────────────────────────┘
 └──────────────┘        staff browsers ──────────┘  (web interface, TCP 443)
```

- **Employee and kiosk Pis always start the connection** and nothing ever
  connects to them. They need no inbound rules, port forwarding or public
  addresses, and can sit behind NAT.
- **One destination, one port:** everything (syncing records, file uploads,
  software updates, live look-ups) goes to the master Pi over HTTPS on a
  single TCP port, **443** by default. (A master set up with the early trial
  kit uses **8443**; check its address on its Pis page.)
- **How often:** every 5 minutes. When the master can't be reached, a Pi
  backs off to every 30 minutes at most, and carries on working offline.
  Its unsent work goes at the next successful sync.
- **No other services:** pi-fleet doesn't contact the internet, cloud
  services or the vendor. The Pis need internet access only while being
  installed, if the operating system is missing a package (see below).

## Security, for your risk assessment

- **Encryption:** TLS 1.2 or newer. The master Pi uses its own **self-signed
  certificate** (ECDSA P-256). Each employee Pi is told the master's
  certificate once, at installation, after a person compares its
  **fingerprint** with the one on the master's Pis page, and from then on
  trusts only that certificate. A public certificate authority isn't
  involved.
- **Authentication:** every request a Pi sends is signed with that Pi's own
  key (HTTP Message Signatures, RFC 9421, Ed25519) and can be used only
  once. A Pi is accepted only after a super user approves it in person,
  with six words the Pi displays, and can be revoked at any time (it then
  erases its records at its next contact).
- **Integrity:** every record is signed by the Pi that made it and chained
  to its previous record; the master re-checks each one, and records it
  doubts are kept and flagged for review, never silently dropped.
- **Data:** equipment, work orders, calibrations, inventory, schedules, and
  user accounts (names, work emails, roles; passwords are stored only as
  Argon2id verifiers). Photos and certificates can be attached. The system
  is designed to hold **no patient information**, and warns users who type
  text that looks like it.
- **At rest:** the master Pi's records are on its external drive, not
  encrypted on the drive itself, so the master needs a physically secure
  place (see below). Backups are encrypted.

## What to set up

### 1. A home for the master Pi

- A locked room or cabinet, on a UPS if possible.
- **Wired Ethernet**, and a **fixed address**: a DHCP reservation or a
  static IP.
- Reachable from every site's Pis (step 3).

### 2. A DNS name

Create a DNS record for the master Pi's address on your internal DNS, for
example `pi-fleet.example.org`. Employee Pis at other sites use this name.

- **Give the name to whoever installs the master Pi before they do.** Its
  setup asks for it ("Network names or addresses for other sites"), because
  employee Pis check the name against the master's certificate and refuse a
  name it doesn't include. You can give several, and IP addresses too.
- Names ending in `.local` (such as `fleet-master.local`) are announced by
  mDNS and only work within one network segment. Don't rely on them across
  routers.
- If the master Pi's IP address changes later, update the DNS record;
  nothing else is needed. If its **name** changes, see step 7.

### 3. Firewall rules

| From | To | Protocol, port | Why |
|---|---|---|---|
| Each site's employee and kiosk Pis | master Pi | TCP 443 | syncing, files, updates |
| Staff computers that use the web interface | master Pi | TCP 443 | web interface in a browser |
| All Pis | your DNS servers | UDP/TCP 53 | the master's name |
| All Pis | your NTP servers (or public NTP) | UDP 123 | correct time; see below |
| All Pis, during installation only | Debian and Raspberry Pi OS package mirrors | TCP 80/443 | only if setup must install a missing OS package |

Nothing is needed **to** the employee or kiosk Pis, and the master Pi needs
no outbound access for pi-fleet.

**Sites without an internal route to the master's site:** use a
site-to-site VPN, or put those Pis on a network that already reaches it.
**Don't publish the master Pi on the public internet.** Every request is
authenticated, but there is no need to take that risk.

### 4. No TLS inspection, no proxy

- **Exempt the master Pi's address from TLS inspection** (SSL decryption on
  firewalls or web proxies). An inspecting device presents its own
  certificate, which the employee Pis rightly refuse, and syncing stops.
- **Employee Pis connect directly,** not through an HTTP proxy. Route their
  traffic to the master without one.

### 5. Network access for the Pis

The Pis are Raspberry Pi 4 or 5 computers running Raspberry Pi OS (Debian),
on Wi-Fi or Ethernet. Connect them as your policy requires for such
devices: an IoT or device VLAN, MAC address registration, WPA2/WPA3
Enterprise, and so on. A good fit is a network that can reach only the
master Pi, DNS and NTP.

**Time:** the Pis should get their time from NTP. Raspberry Pi OS uses
public NTP pool servers by default; if those are blocked, point the Pis at
your own NTP servers (`/etc/systemd/timesyncd.conf`, `NTP=`). With a wrong
clock a Pi still syncs, using the master's time, but its electronic
signatures are flagged for review.

### 6. Test from each site before installing employee Pis

From a laptop at each site, after the master Pi is installed:

```sh
# Is it reachable? Expect "HTTP/... 200".
curl -sk -o /dev/null -w 'HTTP/%{http_version} %{http_code}\n' https://pi-fleet.example.org/login

# Is it really the master, not an inspecting device? Compare the hex digits
# with the fingerprint on the master's Pis page (grouped differently).
openssl s_client -connect pi-fleet.example.org:443 -servername pi-fleet.example.org </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

A browser works too: it should show pi-fleet's sign-in page after warning
about the self-signed certificate.

### 7. Installing employee Pis, and later changes

- **Installing:** on the employee Pi, run the pi-fleet release file and
  choose **Employee Pi** (or **Kiosk Pi**). When it asks for the master Pi,
  enter the DNS name from step 2. It shows the master's fingerprint, which
  the installer compares with the Pis page before going on.
- **Adding a name later** (a new DNS name, or a site that must use an IP
  address): run the release file on the **master Pi** and choose **Add a
  network name or address to the master's certificate**. This makes a new
  certificate with a new fingerprint. Then on **each employee and kiosk
  Pi**, run the release file and choose **Connect to the master again**;
  until then they keep their work but don't sync.
- **Moving the master Pi to another address:** update its DNS record.
  If the Pis use its DNS name, they follow by themselves.

### 8. Phones reporting problems (QR labels)

Each piece of equipment can carry a QR label. Scanning it opens the master
Pi's report page (`https://<master's name>/r/<label>`), where anyone can
report a problem without signing in. For that:

- **Reach:** phones on the hospital Wi-Fi need to reach the master Pi on
  port 443, by the same name the label was printed with. Print labels from
  a browser that opened the master by that name (the DNS name from step 2,
  rather than `.local`, which some phones can't resolve).
- **The certificate:** the master's certificate is its own, so phones show
  a warning the first time unless they trust it. To avoid that, give the
  master a certificate from your institution's certificate authority for
  its DNS name, or install the master's certificate on managed phones.
- **What's exposed:** the report page shows only the equipment's tag,
  model and location, and the status page only a report's progress. It
  never shows who reported what. Reports are limited to 5 per address in
  10 minutes and 120 in all, and the page refuses text that looks like
  patient information. Don't make it reachable from outside the hospital.

## Bandwidth

Small. Each sync is a few kilobytes when there's little new work. A Pi
downloads its working set (the equipment and work relevant to it) when
something has changed, typically a few megabytes at most for a large
institution. Attached files are up to 10 MB each, sent once. Software
updates are about 20 MB per Pi, a few times a year.

## Troubleshooting

| What the employee Pi shows or logs | Likely cause | What to do |
|---|---|---|
| Last sync stays old; "couldn't reach" in setup | no route, firewall, or DNS | test with `curl` from that site (step 6); check rules (step 3) |
| "certificate is valid for …, not …" | the name the Pi uses isn't in the master's certificate | add the name on the master (step 7) |
| "certificate signed by unknown authority", or a fingerprint that doesn't match the Pis page | TLS inspection, or the wrong host | exempt the master from inspection (step 4); check the DNS record |
| "This Pi needs updating" banner | the Pi's pi-fleet version is too old for the master | run Update-Pi on it (`deploy/tools`) |
| "Clock not verified" | the Pi can't reach NTP | allow NTP or set an internal server (step 5) |

The Pi's own log: `journalctl -u pi-fleet`. The master's Pis page lists
every Pi, its version, and whether it needs updating.
