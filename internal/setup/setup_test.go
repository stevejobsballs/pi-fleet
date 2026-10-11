package setup

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"filippo.io/age"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// This Pi 5's own lsblk output: a USB stick (with the release key on it)
// and the SD card the system runs from.
const piDisks = `{"blockdevices": [
 {"name":"loop0","path":"/dev/loop0","type":"loop","size":2147483648,"model":null,"serial":null,"tran":null,"rm":false,"fstype":"swap","label":"origin:rpi-swap","uuid":null,"mountpoints":[]},
 {"name":"sda","path":"/dev/sda","type":"disk","size":31037849600,"model":"STORE N GO","serial":"24101821120329","tran":"usb","rm":true,"fstype":null,"label":null,"uuid":null,"mountpoints":[],
  "children":[{"name":"sda1","path":"/dev/sda1","type":"part","size":31036801024,"fstype":"vfat","label":"USB STICK","uuid":"C80B-5C5B","mountpoints":["/media/clooney/USB STICK"]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":1000204886016,"model":"Samsung SSD T7","serial":"S5","tran":"usb","rm":false,"fstype":null,"label":null,"uuid":null,"mountpoints":[],
  "children":[{"name":"sdb1","path":"/dev/sdb1","type":"part","size":1000203837440,"fstype":"exfat","label":"T7","uuid":"1234-ABCD","mountpoints":[null]}]},
 {"name":"mmcblk0","path":"/dev/mmcblk0","type":"disk","size":127865454592,"model":null,"serial":"0xb483c578","tran":"mmc","rm":false,"fstype":null,"label":null,"uuid":null,"mountpoints":[],
  "children":[{"name":"mmcblk0p1","path":"/dev/mmcblk0p1","type":"part","size":536870912,"fstype":"vfat","label":"bootfs","uuid":"x","mountpoints":["/boot/firmware"]},
              {"name":"mmcblk0p2","path":"/dev/mmcblk0p2","type":"part","size":127328583680,"fstype":"ext4","label":"rootfs","uuid":"y","mountpoints":["/"]}]}
]}`

func TestParseDisksLeavesOutTheSystemDisk(t *testing.T) {
	disks, err := ParseDisks([]byte(piDisks))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range disks {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"sda", "sdb"}) {
		t.Fatalf("disks = %v", names)
	}
	if got := disks[0].Describe(); got != `STORE N GO, 31 GB, USB — "USB STICK" (vfat) open at /media/clooney/USB STICK` {
		t.Errorf("describe = %q", got)
	}
	if got := disks[1].Describe(); got != `Samsung SSD T7, 1.0 TB, USB — "T7" (exfat)` {
		t.Errorf("describe = %q", got)
	}
	readDir := func(dir string) ([]os.DirEntry, error) {
		if dir != "/media/clooney/USB STICK" {
			return nil, fs.ErrNotExist
		}
		return []os.DirEntry{entry("pi-fleet release keys"), entry(".Trash-1000"), entry("photos")}, nil
	}
	if got := disks[0].Contents(readDir); !slices.Equal(got, []string{"/media/clooney/USB STICK: photos, pi-fleet release keys"}) {
		t.Errorf("contents = %q", got)
	}
}

type entry string

func (e entry) Name() string               { return string(e) }
func (e entry) IsDir() bool                { return true }
func (e entry) Type() fs.FileMode          { return fs.ModeDir }
func (e entry) Info() (fs.FileInfo, error) { return nil, nil }

func TestPartitionPath(t *testing.T) {
	for in, want := range map[string]string{"/dev/sda": "/dev/sda1", "/dev/nvme0n1": "/dev/nvme0n1p1", "/dev/mmcblk1": "/dev/mmcblk1p1"} {
		if got := partitionPath(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestUpdateFstab(t *testing.T) {
	old := "proc /proc proc defaults 0 0\nUUID=old /srv/pi-fleet ext4 defaults 0 2\n# /srv/pi-fleet comment\n"
	got := UpdateFstab(old, "abc", "/srv/pi-fleet")
	want := "proc /proc proc defaults 0 0\n# replaced by pi-fleet setup: UUID=old /srv/pi-fleet ext4 defaults 0 2\n# /srv/pi-fleet comment\n" +
		"# pi-fleet master Pi data drive\nUUID=abc  /srv/pi-fleet  ext4  defaults,noatime,nofail,x-systemd.device-timeout=20s  0  2\n"
	if got != want {
		t.Fatalf("fstab:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpdateHosts(t *testing.T) {
	got := UpdateHosts("127.0.0.1\tlocalhost\n127.0.1.1\traspberrypi\n", "fleet-master")
	if got != "127.0.0.1\tlocalhost\n127.0.1.1\tfleet-master\n" {
		t.Fatalf("hosts = %q", got)
	}
	if got := UpdateHosts("127.0.0.1 localhost\n", "x"); got != "127.0.0.1 localhost\n127.0.1.1\tx\n" {
		t.Fatalf("hosts = %q", got)
	}
}

func TestParseMaster(t *testing.T) {
	for in, want := range map[string]string{
		"fleet-master.local":                      "fleet-master.local 443,8443",
		"https://fleet-master.local:8443/admin/x": "fleet-master.local 8443",
		"10.0.0.5:443":                            "10.0.0.5 443",
	} {
		h, p := parseMaster(in)
		if got := h + " " + strings.Join(p, ","); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestCertificateWorksAndFingerprintMatches(t *testing.T) {
	certPEM, keyPEM, err := NewCertificate([]string{"fleet-master.local", "fleet-master"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !browsersAccept(certPEM) {
		t.Fatal("browsers wouldn't accept it")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()
	got, err := FetchCertificate(srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Fingerprint(certPEM)
	b, _ := Fingerprint(got)
	if a != b || len(strings.Fields(a)) != 16 {
		t.Fatalf("fingerprints %q and %q", a, b)
	}
	// An employee Pi trusting it as its CA file can connect by name.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(got)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "fleet-master.local"}}}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// --- the wizard, on a fake machine ---

type fakeSys struct {
	calls  []string
	files  map[string][]byte
	exists map[string]bool
	mounts map[string]bool
	output func(cmd string) ([]byte, error)
	fail   map[string]error // command prefix -> error
}

func newFake() *fakeSys {
	return &fakeSys{files: map[string][]byte{"/etc/fstab": []byte("proc /proc proc defaults 0 0\n"), "/etc/hosts": []byte("127.0.1.1\traspberrypi\n")},
		exists: map[string]bool{"/run/systemd/system": true}, mounts: map[string]bool{}}
}

func (f *fakeSys) Run(name string, args ...string) error {
	c := shellJoin(append([]string{name}, args...))
	f.calls = append(f.calls, c)
	for prefix, err := range f.fail {
		if strings.HasPrefix(c, prefix) {
			return err
		}
	}
	switch name {
	case "mount":
		f.mounts[args[len(args)-1]] = true
	case "umount":
		delete(f.mounts, args[0])
	}
	if name == "runuser" && slices.Contains(args, "init") {
		dir := args[slices.Index(args, "-data")+1]
		f.exists[dir+"/pi-fleet.db"] = true
	}
	return nil
}

func (f *fakeSys) Output(name string, args ...string) ([]byte, error) {
	c := shellJoin(append([]string{name}, args...))
	if name == "mountpoint" {
		if f.mounts[args[1]] {
			return nil, nil
		}
		return nil, errors.New("not a mountpoint")
	}
	if f.output != nil {
		return f.output(c)
	}
	return nil, errors.New("no output")
}

func (f *fakeSys) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.files[path] = data
	return nil
}
func (f *fakeSys) ReadFile(path string) ([]byte, error) {
	if b, ok := f.files[path]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}
func (f *fakeSys) Exists(path string) bool { _, ok := f.files[path]; return ok || f.exists[path] }
func (f *fakeSys) MkdirAll(path string, mode os.FileMode) error {
	f.exists[path] = true
	return nil
}
func (f *fakeSys) Rename(from, to string) error {
	f.calls = append(f.calls, "mv "+from+" "+to)
	return nil
}
func (f *fakeSys) Symlink(target, link string) error {
	f.calls = append(f.calls, "ln -sfn "+target+" "+link)
	return nil
}
func (f *fakeSys) CopyFile(from, to string, mode os.FileMode) error {
	f.calls = append(f.calls, fmt.Sprintf("install %s %s", from, to))
	return nil
}

func (f *fakeSys) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func wizard(sys *fakeSys, answers ...string) (*Wizard, *bytes.Buffer) {
	var out bytes.Buffer
	lookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	fileExists = func(string) bool { return true }
	w := &Wizard{
		UI: NewUI(strings.NewReader(strings.Join(answers, "\n")+"\n"), &out), Sys: sys,
		Self: "/home/pi/Downloads/pi-fleet_v1.0.0_linux_arm64", Version: "v1.0.0",
		Now:     func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) },
		ReadDir: func(string) ([]os.DirEntry, error) { return []os.DirEntry{entry("pi-fleet release keys")}, nil },
		Addrs:   func() []net.IP { return []net.IP{net.ParseIP("10.0.0.5")} },
		Health:  func(string) error { return nil },
		Sleep:   func(time.Duration) {},
	}
	return w, &out
}

func outputs(sys *fakeSys, m map[string]string) {
	sys.output = func(c string) ([]byte, error) {
		for prefix, out := range m {
			if strings.HasPrefix(c, prefix) {
				return []byte(out), nil
			}
		}
		return nil, errors.New("no output for " + c)
	}
}

func TestNewMasterOnAnErasedDrive(t *testing.T) {
	sys := newFake()
	outputs(sys, map[string]string{"lsblk": piDisks, "hostname": "raspberrypi\n", "blkid": "new-uuid\n", "id pifleet": "uid=999"})
	w, out := wizard(sys,
		"1",               // master Pi
		"",                // name: fleet-master
		"",                // drive plugged in
		"1", "y", "erase", // the USB stick, as a trial, but ERASE not typed in capitals: back to the list
		"",           // drive plugged in
		"2", "ERASE", // the SSD
		"",       // no network names for other sites
		"Jsmith", // not lowercase
		"jsmith", "Jo Smith", "jo at example", "jo@example.org",
		"n", // backups later
	)
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if sys.ran("wipefs --all --quiet /dev/sda") || sys.ran("mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sda1") {
		t.Fatalf("erased the USB stick:\n%s", strings.Join(sys.calls, "\n"))
	}
	for _, want := range []string{
		"hostnamectl set-hostname fleet-master",
		"wipefs --all --quiet /dev/sdb",
		"parted --script /dev/sdb mklabel gpt",
		"mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sdb1",
		"mount /srv/pi-fleet",
		"install /home/pi/Downloads/pi-fleet_v1.0.0_linux_arm64 /opt/pi-fleet/releases/v1.0.0/pi-fleet",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet init -data /srv/pi-fleet -role central",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet bootstrap -data /srv/pi-fleet -username jsmith -name 'Jo Smith' -email jo@example.org",
		"systemctl enable pi-fleet",
	} {
		if !sys.ran(want) {
			t.Errorf("didn't run %q", want)
		}
	}
	if !strings.Contains(string(sys.files["/etc/fstab"]), "UUID=new-uuid  /srv/pi-fleet  ext4  defaults,noatime,nofail") {
		t.Errorf("fstab:\n%s", sys.files["/etc/fstab"])
	}
	unit := string(sys.files[UnitPath])
	for _, want := range []string{"RequiresMountsFor=/srv/pi-fleet", "ExecStartPre=/usr/bin/mountpoint -q /srv/pi-fleet", "-tls-cert /srv/pi-fleet/tls/cert.pem", "-listen :443"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if _, ok := sys.files["/srv/pi-fleet/tls/key.pem"]; !ok {
		t.Error("no certificate on the data drive")
	}
	text := out.String()
	for _, want := range []string{"looks like a USB stick", "only for testing", "pi-fleet release keys", "release signing key", "may be a USB stick", "https://fleet-master.local", "Certificate fingerprint"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestNewMasterReusesADataDrive(t *testing.T) {
	sys := newFake()
	disks := strings.Replace(piDisks, `"fstype":"exfat","label":"T7","uuid":"1234-ABCD"`, `"fstype":"ext4","label":"PIFLEET-DATA","uuid":"keep-me"`, 1)
	outputs(sys, map[string]string{"lsblk": disks, "hostname": "fleet-master\n", "id pifleet": "uid=999",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet bootstrap -data /srv/pi-fleet -check": "this master Pi has a super user\n"})
	sys.exists["/srv/pi-fleet/pi-fleet.db"] = true // on the drive, once mounted
	sys.files["/srv/pi-fleet/tls/cert.pem"], _, _ = NewCertificate([]string{"fleet-master.local"}, nil, time.Now())
	w, out := wizard(sys, "1", "", "", "2", "y", "", "n")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if sys.ran("wipefs") || sys.ran("mkfs") || sys.ran("runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet init") || sys.ran("runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet bootstrap -data /srv/pi-fleet -username") {
		t.Fatalf("changed the reused drive:\n%s", strings.Join(sys.calls, "\n"))
	}
	if !strings.Contains(string(sys.files["/etc/fstab"]), "UUID=keep-me") || !strings.Contains(out.String(), "Found the master Pi's records") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestMoveAnExistingMasterToADrive(t *testing.T) {
	sys := newFake()
	sys.exists[DataDir+"/pi-fleet.db"] = true
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443 -tls-cert /etc/pi-fleet/tls/cert.pem\n")
	sys.files["/etc/pi-fleet/tls/cert.pem"] = []byte("x")
	outputs(sys, map[string]string{"lsblk": piDisks, "blkid": "new-uuid\n"})
	w, out := wizard(sys, "1", "", "2", "ERASE")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	order := []string{
		"mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sdb1",
		"mount /dev/sdb1 /mnt/pi-fleet-move",
		"systemctl stop pi-fleet",
		"cp -a /srv/pi-fleet/. /mnt/pi-fleet-move/",
		"cp -a /etc/pi-fleet/tls /mnt/pi-fleet-move/tls",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet selfcheck -data /mnt/pi-fleet-move",
		"umount /mnt/pi-fleet-move",
		"mv /srv/pi-fleet /srv/pi-fleet.on-sd-card-2026-10-07",
		"mount /srv/pi-fleet",
		"systemctl restart pi-fleet",
	}
	i := 0
	for _, c := range sys.calls {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i != len(order) {
		t.Fatalf("stopped matching at %q; ran:\n%s", order[i], strings.Join(sys.calls, "\n"))
	}
	if !strings.Contains(string(sys.files[UnitPath]), "-listen :8443") {
		t.Fatal("the port changed; employee Pis would lose the master")
	}
}

func TestMoveStopsSafelyIfTheCopyFailsItsCheck(t *testing.T) {
	sys := newFake()
	sys.exists[DataDir+"/pi-fleet.db"] = true
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	outputs(sys, map[string]string{"lsblk": piDisks, "blkid": "new-uuid\n"})
	sys.fail = map[string]error{"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet selfcheck": errors.New("exit status 1")}
	w, out := wizard(sys, "1", "", "2", "ERASE")
	err := w.Run()
	if err == nil || !strings.Contains(err.Error(), "running from the SD card as before") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if sys.ran("mv /srv/pi-fleet") || strings.Contains(string(sys.files["/etc/fstab"]), "new-uuid") {
		t.Fatal("switched over after a failed check")
	}
	if !sys.ran("systemctl start pi-fleet") {
		t.Fatal("pi-fleet left stopped")
	}
}

func TestEmployeePi(t *testing.T) {
	sys := newFake()
	status := "not activated"
	sys.output = func(c string) ([]byte, error) {
		switch {
		case strings.HasPrefix(c, "hostname"):
			return []byte("raspberrypi\n"), nil
		case strings.Contains(c, "activation-status"):
			s := status
			status = "active" // approved on the second check
			return []byte(s + "\n"), nil
		}
		return nil, errors.New("no")
	}
	cert, _, _ := NewCertificate([]string{"fleet-master.local"}, nil, time.Now())
	var tried []string
	w, out := wizard(sys, "2", "", "fleet-master.local", "n", "fleet-master.local", "y", "tess")
	w.SudoUser = "tess"
	w.Fetch = func(hp string) ([]byte, error) {
		tried = append(tried, hp)
		if hp == "fleet-master.local:443" {
			return nil, errors.New("connection refused")
		}
		return cert, nil
	}
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !slices.Equal(tried[:2], []string{"fleet-master.local:443", "fleet-master.local:8443"}) {
		t.Errorf("tried %v", tried)
	}
	if !bytes.Equal(sys.files[MasterCA], cert) {
		t.Fatal("master certificate not saved")
	}
	for _, want := range []string{
		"hostnamectl set-hostname fleet-tess",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet init -data /var/lib/pi-fleet -role node",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet activate -data /var/lib/pi-fleet -central https://fleet-master.local:8443 -ca /etc/pi-fleet/master.pem -username tess",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet sync -data /var/lib/pi-fleet",
	} {
		if !sys.ran(want) {
			t.Errorf("didn't run %q\n%s", want, strings.Join(sys.calls, "\n"))
		}
	}
	if sys.ran("wipefs") || sys.ran("parted") {
		t.Fatal("touched a drive on an employee Pi")
	}
	if !strings.Contains(string(sys.files[UnitPath]), "run -data /var/lib/pi-fleet") {
		t.Fatalf("unit:\n%s", sys.files[UnitPath])
	}
}

func TestRefusesDevelopmentBuildsAndDowngrades(t *testing.T) {
	sys := newFake()
	w, _ := wizard(sys)
	w.Version = "dev"
	if err := w.Run(); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("err = %v", err)
	}
	outputs(sys, map[string]string{"/opt/pi-fleet/current/pi-fleet version": "pi-fleet v2.0.0\n", "id pifleet": "uid=999"})
	w, _ = wizard(sys)
	if err := w.program(NodeDataDir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateAnExistingMasterUsesTheVerifiedUpdate(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :443\n")
	sys.mounts[DataDir] = true
	w, out := wizard(sys, "4")
	w.Self = "/home/pi/Downloads/v1.0.0/pi-fleet_v1.0.0_linux_arm64"
	// Without the signature files next to it: nothing happens.
	if err := w.Run(); !errors.Is(err, ErrCancelled) || sys.ran("systemctl stop") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, f := range []string{"manifest.json", "manifest.json.minisig"} {
		sys.files["/home/pi/Downloads/v1.0.0/"+f] = []byte("x")
	}
	w, out = wizard(sys, "4")
	w.Self = "/home/pi/Downloads/v1.0.0/pi-fleet_v1.0.0_linux_arm64"
	w.ReadDir = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{entry("manifest.json"), entry("manifest.json.minisig"), entry("pi-fleet_v1.0.0_linux_arm64"), entry("notes.txt")}, nil
	}
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"systemctl stop pi-fleet",
		"/opt/pi-fleet/current/pi-fleet update -data /srv/pi-fleet -root /opt/pi-fleet -from /home/pi/Downloads/v1.0.0",
		"install /home/pi/Downloads/v1.0.0/pi-fleet_v1.0.0_linux_arm64 /srv/pi-fleet/releases/pi-fleet_v1.0.0_linux_arm64",
		"systemctl restart pi-fleet",
	} {
		if !sys.ran(want) {
			t.Errorf("didn't run %q\n%s", want, strings.Join(sys.calls, "\n"))
		}
	}
	if sys.ran("install /home/pi/Downloads/v1.0.0/notes.txt") || sys.ran("install /home/pi/Downloads/v1.0.0/pi-fleet_v1.0.0_linux_arm64 /opt") {
		t.Fatal("copied the program around the verified update")
	}
}

func TestNothingToUpdateWhenTheVersionIsInstalled(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	outputs(sys, map[string]string{"/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	// Records on the SD card: the move is offered, not an update.
	w, out := wizard(sys, "3") // stop
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "Update to this version") || !strings.Contains(out.String(), "Move the records") {
		t.Fatalf("output:\n%s", out)
	}
	// On a drive already: no update offered either.
	sys.mounts[DataDir] = true
	w, out = wizard(sys, "4") // stop
	if err := w.Run(); err != nil || !strings.Contains(out.String(), "1) Move the records to a different drive") || strings.Contains(out.String(), "Update to") || sys.ran("systemctl") {
		t.Fatalf("err %v\n%s", err, out)
	}
}

// The records are on a USB stick (sda1, open at /srv/pi-fleet); an SSD
// (sdb) has been plugged in to replace it.
const stickAndSSD = `{"blockdevices": [
 {"name":"sda","path":"/dev/sda","type":"disk","size":31037849600,"model":"STORE N GO","tran":"usb","rm":true,"fstype":null,"mountpoints":[],
  "children":[{"name":"sda1","path":"/dev/sda1","type":"part","size":31036801024,"fstype":"ext4","label":"PIFLEET-DATA","uuid":"old-uuid","mountpoints":["/srv/pi-fleet"]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":1000204886016,"model":"Samsung SSD T7","tran":"usb","rm":false,"fstype":null,"mountpoints":[],
  "children":[{"name":"sdb1","path":"/dev/sdb1","type":"part","size":1000203837440,"fstype":"exfat","label":"T7","uuid":"1234-ABCD","mountpoints":[null]}]},
 {"name":"mmcblk0","path":"/dev/mmcblk0","type":"disk","size":127865454592,"tran":"mmc","fstype":null,"mountpoints":[],
  "children":[{"name":"mmcblk0p2","path":"/dev/mmcblk0p2","type":"part","size":127328583680,"fstype":"ext4","label":"rootfs","uuid":"y","mountpoints":["/"]}]}
]}`

func TestMoveToADifferentDrive(t *testing.T) {
	sys := newFake()
	sys.exists[DataDir+"/pi-fleet.db"] = true
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443 -tls-cert /srv/pi-fleet/tls/cert.pem\n")
	sys.files["/etc/fstab"] = []byte("proc /proc proc defaults 0 0\n" + FstabLine("old-uuid", DataDir) + "\n")
	sys.mounts[DataDir] = true
	outputs(sys, map[string]string{"lsblk": stickAndSSD, "blkid": "new-uuid\n", "findmnt -n -o SOURCE /srv/pi-fleet": "/dev/sda1\n",
		"/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	// Only the SSD is offered: the stick holding the records is not.
	w, out := wizard(sys, "1", "", "1", "ERASE")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "STORE N GO") {
		t.Fatalf("offered the drive the records are on:\n%s", out)
	}
	if sys.ran("wipefs --all --quiet /dev/sda") || sys.ran("mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sda1") {
		t.Fatal("erased the current drive")
	}
	order := []string{
		"mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sdb1",
		"mount /dev/sdb1 /mnt/pi-fleet-move",
		"systemctl stop pi-fleet",
		"cp -a /srv/pi-fleet/. /mnt/pi-fleet-move/",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet selfcheck -data /mnt/pi-fleet-move",
		"umount /mnt/pi-fleet-move",
		"umount /srv/pi-fleet",
		"mount /srv/pi-fleet",
		"e2label /dev/sda1 PIFLEET-OLD",
		"systemctl restart pi-fleet",
	}
	i := 0
	for _, c := range sys.calls {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i != len(order) {
		t.Fatalf("stopped matching at %q; ran:\n%s", order[i], strings.Join(sys.calls, "\n"))
	}
	fstab := string(sys.files["/etc/fstab"])
	if !strings.Contains(fstab, "# replaced by pi-fleet setup: UUID=old-uuid") || !strings.Contains(fstab, "\n"+FstabLine("new-uuid", DataDir)) {
		t.Fatalf("fstab:\n%s", fstab)
	}
	if sys.ran("mv /srv/pi-fleet") {
		t.Fatal("renamed the mount point")
	}
	if !strings.Contains(out.String(), "PIFLEET-OLD") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDriveMoveStopsSafelyIfTheCopyFailsItsCheck(t *testing.T) {
	sys := newFake()
	sys.exists[DataDir+"/pi-fleet.db"] = true
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	before := "proc /proc proc defaults 0 0\n" + FstabLine("old-uuid", DataDir) + "\n"
	sys.files["/etc/fstab"] = []byte(before)
	sys.mounts[DataDir] = true
	outputs(sys, map[string]string{"lsblk": stickAndSSD, "blkid": "new-uuid\n", "findmnt -n -o SOURCE /srv/pi-fleet": "/dev/sda1\n"})
	sys.fail = map[string]error{"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet selfcheck": errors.New("exit status 1")}
	w, out := wizard(sys, "1", "", "1", "ERASE")
	err := w.Run()
	if err == nil || !strings.Contains(err.Error(), "running from the drive they are on now as before") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if sys.ran("umount /srv/pi-fleet") || sys.ran("e2label") || string(sys.files["/etc/fstab"]) != before {
		t.Fatalf("switched over after a failed check:\n%s", strings.Join(sys.calls, "\n"))
	}
	if !sys.ran("systemctl start pi-fleet") {
		t.Fatal("pi-fleet left stopped")
	}
}

// The records are on sda; sdb becomes the backup drive; sdc is a USB stick
// for the keys; sdd and sde become the off-site disks.
const backupDisks = `{"blockdevices": [
 {"name":"sda","path":"/dev/sda","type":"disk","size":1000204886016,"model":"Data SSD","tran":"usb","fstype":null,"mountpoints":[],
  "children":[{"name":"sda1","path":"/dev/sda1","type":"part","size":1000203837440,"fstype":"ext4","label":"PIFLEET-DATA","uuid":"data-uuid","mountpoints":["/srv/pi-fleet"]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":1000204886016,"model":"Samsung SSD T7","tran":"usb","fstype":null,"mountpoints":[],
  "children":[{"name":"sdb1","path":"/dev/sdb1","type":"part","size":1000203837440,"fstype":"exfat","label":"T7","uuid":"1234-ABCD","mountpoints":[null]}]},
 {"name":"sdc","path":"/dev/sdc","type":"disk","size":31037849600,"model":"Key Stick","tran":"usb","rm":true,"fstype":null,"mountpoints":[],
  "children":[{"name":"sdc1","path":"/dev/sdc1","type":"part","size":31036801024,"fstype":"vfat","label":"KEYS","uuid":"K-1","mountpoints":["/media/clooney/KEYS"]}]},
 {"name":"sdd","path":"/dev/sdd","type":"disk","size":500107862016,"model":"Offsite One","tran":"usb","fstype":null,"mountpoints":[],"children":[]},
 {"name":"sde","path":"/dev/sde","type":"disk","size":500107862016,"model":"Offsite Two","tran":"usb","fstype":null,"mountpoints":[],"children":[]},
 {"name":"mmcblk0","path":"/dev/mmcblk0","type":"disk","size":127865454592,"tran":"mmc","fstype":null,"mountpoints":[],
  "children":[{"name":"mmcblk0p2","path":"/dev/mmcblk0p2","type":"part","size":127328583680,"fstype":"ext4","label":"rootfs","uuid":"y","mountpoints":["/"]}]}
]}`

func TestSetUpBackupsOnAnExistingMaster(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443 (trial)\n")
	sys.mounts[DataDir] = true
	outputs(sys, map[string]string{"lsblk": backupDisks, "blkid": "new-uuid\n", "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys,
		"2",              // set up backups
		"", "1", "ERASE", // backup drive: the T7
		"1", "1", // your key: new, saved on the key stick
		"1", "1", // escrow key: new, on the key stick
		"n",              // no more key holders
		"2",              // two off-site disks
		"", "3", "ERASE", // OFFSITE-A: Offsite One
		"", "4", "ERASE", // OFFSITE-B: Offsite Two
	)
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, d := range []string{"sda", "sdc"} {
		if sys.ran("wipefs --all --quiet /dev/"+d) || sys.ran("mkfs.ext4 -q -F -L PIFLEET-BACKUP -m 1 /dev/"+d) {
			t.Fatalf("erased %s:\n%s", d, strings.Join(sys.calls, "\n"))
		}
	}
	for _, want := range []string{
		"mkfs.ext4 -q -F -L PIFLEET-BACKUP -m 1 /dev/sdb1",
		"mount /srv/pi-fleet-backup",
		"chown pifleet:pifleet /srv/pi-fleet-backup",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet backup-config -data /srv/pi-fleet -dir /srv/pi-fleet-backup -recipient age1",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet backup-now -data /srv/pi-fleet",
		"rm -f " + OffsiteRulePath,
		"mkfs.ext4 -q -F -L OFFSITE-A -m 1 /dev/sdd1",
		"mount /media/OFFSITE-A",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet offsite-register -data /srv/pi-fleet -disk /media/OFFSITE-A -label OFFSITE-A",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet offsite-write -data /srv/pi-fleet -disk /media/OFFSITE-A",
		"umount /media/OFFSITE-A",
		"mkfs.ext4 -q -F -L OFFSITE-B -m 1 /dev/sde1",
		"udevadm control --reload-rules",
		"systemctl restart pi-fleet",
	} {
		if !sys.ran(want) {
			t.Errorf("didn't run %q", want)
		}
	}
	// Both keys are on the stick, and the backups are encrypted to them.
	var config string
	for _, c := range sys.calls {
		if strings.Contains(c, " backup-config ") {
			config = c
		}
	}
	for _, f := range []string{"/media/clooney/KEYS/pi-fleet-backup-key-you.txt", "/media/clooney/KEYS/pi-fleet-backup-key-the-sealed-envelope-escrow.txt"} {
		key := string(sys.files[f])
		if !strings.Contains(key, "AGE-SECRET-KEY-1") {
			t.Fatalf("%s:\n%s", f, key)
		}
		// The file is a standard key file: it decrypts what is encrypted
		// to the recipient given to backup-config.
		ids, err := age.ParseIdentities(strings.NewReader(key))
		if err != nil || len(ids) != 1 {
			t.Fatalf("%s isn't a usable key file: %v", f, err)
		}
		pub := ids[0].(*age.X25519Identity).Recipient().String()
		if !strings.Contains(config, "-recipient "+pub) || !strings.Contains(key, "# public key: "+pub) {
			t.Errorf("backups aren't encrypted to the key in %s", f)
		}
		var sealed bytes.Buffer
		wr, _ := age.Encrypt(&sealed, ids[0].(*age.X25519Identity).Recipient())
		wr.Write([]byte("backup"))
		wr.Close()
		if r, err := age.Decrypt(&sealed, ids...); err != nil || r == nil {
			t.Fatalf("key from %s can't decrypt: %v", f, err)
		}
	}
	if strings.Count(config, "-recipient") != 2 {
		t.Errorf("backup-config: %s", config)
	}
	fstab := string(sys.files["/etc/fstab"])
	for _, want := range []string{FstabLine("new-uuid", BackupDir), OffsiteFstabLine("OFFSITE-A"), OffsiteFstabLine("OFFSITE-B")} {
		if !strings.Contains(fstab, want) {
			t.Errorf("fstab lacks %q:\n%s", want, fstab)
		}
	}
	unit := string(sys.files[OffsiteUnitPath])
	if !strings.Contains(unit, "RequiresMountsFor=/media/%i") || !strings.Contains(unit, "-disk /media/%i") || strings.Contains(unit, "%I") {
		t.Errorf("off-site unit:\n%s", unit)
	}
	if !strings.Contains(string(sys.files[OffsiteRulePath]), `ENV{ID_FS_LABEL}=="OFFSITE-*"`) {
		t.Error("no udev rule")
	}
	if !strings.Contains(string(sys.files[UnitPath]), "ReadWritePaths=/srv/pi-fleet -/srv/pi-fleet-backup") {
		t.Errorf("service can't write backups:\n%s", sys.files[UnitPath])
	}
	if !strings.Contains(out.String(), "offsite-confirm") {
		t.Error("no rotation instructions")
	}
}

func TestSlug(t *testing.T) {
	if got := slug("The sealed envelope (escrow)"); got != "the-sealed-envelope-escrow" {
		t.Fatal(got)
	}
}

func TestAddOffsiteDisksLater(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(MasterUnit(443))
	sys.files[BackupDir+"/.pi-fleet-backup-drive"] = []byte("x")
	sys.files["/etc/fstab"] = []byte(OffsiteFstabLine("OFFSITE-A") + "\n" + OffsiteFstabLine("OFFSITE-B") + "\n")
	sys.mounts[DataDir], sys.mounts[BackupDir] = true, true
	outputs(sys, map[string]string{"lsblk": backupDisks, "blkid": "new-uuid\n", "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys, "2", "1", "", "4", "ERASE")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if sys.ran("runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet backup-config") || !sys.ran("mkfs.ext4 -q -F -L OFFSITE-C -m 1 /dev/sde1") {
		t.Fatalf("ran:\n%s", strings.Join(sys.calls, "\n"))
	}
	if !sys.ran("udevadm control --reload-rules") || sys.files[OffsiteRulePath] == nil {
		t.Fatal("automatic write not reinstalled")
	}
}

func TestTrialInstallationOnAUSBStick(t *testing.T) {
	sys := newFake()
	outputs(sys, map[string]string{"lsblk": piDisks, "hostname": "fleet-master\n", "blkid": "new-uuid\n", "id pifleet": "uid=999"})
	w, out := wizard(sys,
		"1", "",
		"", "1", "n", // the stick: not for a trial, so back to the list
		"", "1", "y", "ERASE", // the stick, as a trial
		"", "jsmith", "Jo Smith", "jo@example.org", "n")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := sys.files[DataDir+"/"+TrialMarker]; !ok {
		t.Fatal("not marked as a trial")
	}
	if !strings.Contains(out.String(), "Don't use a USB stick for actual work") || !strings.Contains(out.String(), "TRIAL INSTALLATION") {
		t.Fatalf("output:\n%s", out)
	}
	// The SSD is no trial.
	sys2 := newFake()
	outputs(sys2, map[string]string{"lsblk": piDisks, "hostname": "fleet-master\n", "blkid": "new-uuid\n", "id pifleet": "uid=999"})
	w, out = wizard(sys2, "1", "", "", "2", "ERASE", "", "jsmith", "Jo Smith", "jo@example.org", "n")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := sys2.files[DataDir+"/"+TrialMarker]; ok || strings.Contains(out.String(), "TRIAL") {
		t.Fatal("an SSD install marked as a trial")
	}
}

func TestMovingOffTheStickEndsTheTrial(t *testing.T) {
	sys := newFake()
	sys.exists[DataDir+"/pi-fleet.db"] = true
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	sys.files[DataDir+"/"+TrialMarker] = []byte("x")
	sys.mounts[DataDir] = true
	outputs(sys, map[string]string{"lsblk": stickAndSSD, "blkid": "new-uuid\n", "findmnt -n -o SOURCE /srv/pi-fleet": "/dev/sda1\n",
		"/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys, "1", "", "1", "ERASE")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !sys.ran("rm -f /mnt/pi-fleet-move/"+TrialMarker) || !strings.Contains(out.String(), "no longer a trial") {
		t.Fatalf("trial mark kept on the SSD:\n%s\n%s", strings.Join(sys.calls, "\n"), out)
	}
}

func TestAnExistingStickInstallIsMarkedAsATrial(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	sys.mounts[DataDir] = true
	outputs(sys, map[string]string{"lsblk": stickAndSSD, "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys, "4") // stop
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := sys.files[DataDir+"/"+TrialMarker]; !ok || !sys.ran("systemctl restart pi-fleet") || !strings.Contains(out.String(), "TRIAL INSTALLATION") {
		t.Fatalf("not marked:\n%s", out)
	}
}

// What happened on the trial master: the data stick dropped out, came
// back unopened, and setup was run.
func TestMissingDataDriveIsNeverTakenForRecordsOnTheSDCard(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(MasterUnit(8443))
	sys.files["/etc/fstab"] = []byte("proc /proc proc defaults 0 0\n" + FstabLine("old-uuid", DataDir) + "\n")
	// The stick is back, unopened, with the same file system.
	disks := strings.Replace(stickAndSSD, `"mountpoints":["/srv/pi-fleet"]`, `"mountpoints":[]`, 1)
	outputs(sys, map[string]string{"lsblk": disks, "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	sys.exists["/dev/disk/by-uuid/old-uuid"] = true
	w, out := wizard(sys, "4") // stop
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !sys.ran("mount /srv/pi-fleet") || !sys.ran("systemctl restart pi-fleet") || !strings.Contains(out.String(), "Found the data drive") {
		t.Fatalf("didn't reopen the drive:\n%s\n%s", strings.Join(sys.calls, "\n"), out)
	}
	if strings.Contains(out.String(), "on the SD card") || sys.ran("wipefs") {
		t.Fatalf("took the missing drive for records on the SD card:\n%s", out)
	}
}

func TestDataDriveOfferedForErasingNever(t *testing.T) {
	sys := newFake()
	sys.files["/etc/fstab"] = []byte(FstabLine("old-uuid", DataDir) + "\n")
	disks := strings.Replace(stickAndSSD, `"mountpoints":["/srv/pi-fleet"]`, `"mountpoints":[]`, 1)
	outputs(sys, map[string]string{"lsblk": disks})
	w, out := wizard(sys, "1")
	w.defaults()
	d, err := w.pickDisk("Which?")
	if err != nil || d == nil || d.Name != "sdb" || strings.Contains(out.String(), "STORE N GO") {
		t.Fatalf("offered %v:\n%s", d, out)
	}
}

func TestRestoreFromTheSDCardCopyWhenTheDriveIsLost(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(MasterUnit(8443))
	sys.files["/etc/fstab"] = []byte("proc /proc proc defaults 0 0\n" + FstabLine("old-uuid", DataDir) + "\n")
	copy := "/srv/pi-fleet.on-sd-card-2026-10-07"
	sys.exists[copy] = true
	sys.exists[DataDir] = true
	// The data stick was erased: the old file system is gone for good.
	disks := strings.Replace(stickAndSSD, `"label":"PIFLEET-DATA","uuid":"old-uuid","mountpoints":["/srv/pi-fleet"]`, `"label":"","uuid":"erased","mountpoints":[]`, 1)
	outputs(sys, map[string]string{"lsblk": disks, "blkid": "new-uuid\n", "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys,
		"2", // restore from the SD card copy
		"y",
		"", "2", "ERASE", // put them on the SSD
	)
	w.ReadDir = func(dir string) ([]os.DirEntry, error) {
		switch dir {
		case "/srv":
			return []os.DirEntry{entry("pi-fleet"), entry("pi-fleet.on-sd-card-2026-10-07"), entry("other")}, nil
		case DataDir:
			return nil, nil // the empty mount point
		}
		return []os.DirEntry{}, nil
	}
	// cp -a puts the records in place.
	orig := sys.output
	sys.output = func(c string) ([]byte, error) { return orig(c) }
	w.Sys = &restoreFake{sys}
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	order := []string{
		"systemctl stop pi-fleet",
		"rmdir /srv/pi-fleet",
		"cp -a /srv/pi-fleet.on-sd-card-2026-10-07 /srv/pi-fleet",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet selfcheck -data /srv/pi-fleet",
		"mkfs.ext4 -q -F -L PIFLEET-DATA -m 1 /dev/sdb1",
		"cp -a /srv/pi-fleet/. /mnt/pi-fleet-move/",
		"mv /srv/pi-fleet /srv/pi-fleet.on-sd-card-2026-10-07-120000",
		"mount /srv/pi-fleet",
		"systemctl restart pi-fleet",
	}
	i := 0
	for _, c := range sys.calls {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i != len(order) {
		t.Fatalf("stopped matching at %q; ran:\n%s", order[i], strings.Join(sys.calls, "\n"))
	}
	if sys.ran("mv /srv/pi-fleet.on-sd-card-2026-10-07 ") || sys.ran("rm -r") {
		t.Fatal("touched the SD card copy")
	}
	if !strings.Contains(string(sys.files["/etc/fstab"]), "# drive lost, removed by pi-fleet setup: UUID=old-uuid") {
		t.Fatalf("fstab:\n%s", sys.files["/etc/fstab"])
	}
}

// restoreFake makes cp -a of the SD card copy create the records.
type restoreFake struct{ *fakeSys }

func (r *restoreFake) Run(name string, args ...string) error {
	if name == "cp" && len(args) == 3 && args[2] == DataDir {
		r.exists[DataDir+"/pi-fleet.db"] = true
	}
	return r.fakeSys.Run(name, args...)
}

func TestMoveRefusesWhenThereAreNoRecords(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte("ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :8443\n")
	outputs(sys, map[string]string{"lsblk": piDisks})
	w, out := wizard(sys, "1")
	if err := w.Run(); err == nil || !strings.Contains(err.Error(), "no records") || sys.ran("wipefs") {
		t.Fatalf("err = %v\n%s", err, out)
	}
}

func TestParseNetworkNames(t *testing.T) {
	names, ips, err := parseNetworkNames("pi-fleet.Example.org, https://10.20.30.40:443/ fleet-master")
	if err != nil || !slices.Equal(names, []string{"pi-fleet.example.org", "fleet-master"}) || len(ips) != 1 || ips[0].String() != "10.20.30.40" {
		t.Fatalf("%v %v %v", names, ips, err)
	}
	if _, _, err := parseNetworkNames("pi fleet!"); err == nil {
		t.Fatal("accepted a bad name")
	}
}

func TestNewMasterWithANetworkNameForOtherSites(t *testing.T) {
	sys := newFake()
	outputs(sys, map[string]string{"lsblk": piDisks, "hostname": "fleet-master\n", "blkid": "new-uuid\n", "id pifleet": "uid=999"})
	w, out := wizard(sys, "1", "", "", "2", "ERASE", "pi-fleet.example.org, 10.20.30.40", "jsmith", "Jo Smith", "jo@example.org", "n")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	names, ips, err := certNames(sys.files[DataDir+"/tls/cert.pem"])
	if err != nil || !slices.Contains(names, "pi-fleet.example.org") || !slices.Contains(names, "fleet-master.local") || len(ips) != 2 {
		t.Fatalf("certificate covers %v %v (%v)", names, ips, err)
	}
	if !strings.Contains(out.String(), "https://pi-fleet.example.org") {
		t.Fatalf("done screen should give the network name:\n%s", out)
	}
}

func TestAddANetworkNameToAnExistingMaster(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(MasterUnit(443))
	sys.mounts[DataDir] = true
	old, _, _ := NewCertificate([]string{"fleet-master.local", "fleet-master"}, nil, time.Now())
	sys.files[DataDir+"/tls/cert.pem"] = old
	outputs(sys, map[string]string{"lsblk": backupDisks, "/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	w, out := wizard(sys, "3", "pi-fleet.example.org", "y")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	names, _, _ := certNames(sys.files[DataDir+"/tls/cert.pem"])
	if !slices.Equal(names, []string{"fleet-master.local", "fleet-master", "pi-fleet.example.org"}) {
		t.Fatalf("names = %v", names)
	}
	if _, ok := sys.files[DataDir+"/tls/key.pem"]; !ok || !sys.ran("systemctl restart pi-fleet") || !strings.Contains(out.String(), "Connect to the master again") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestFinishAnEmployeePiThatWasNeverApproved(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(NodeUnit())
	sys.files[NodeDataDir+"/pi-fleet.db"] = []byte("x")
	checks := 0
	sys.output = func(c string) ([]byte, error) {
		switch {
		case strings.HasPrefix(c, "hostname"):
			return []byte("biomedshop\n"), nil
		case strings.Contains(c, "activation-status"):
			if checks++; checks <= 2 { // setup's check, then activate's
				return nil, errors.New("runuser: exit status 1: pi-fleet: this Pi is not activated: store: not found")
			}
			return []byte("active\n"), nil
		case strings.HasPrefix(c, "id pifleet"):
			return []byte("uid=999\n"), nil
		}
		return nil, errors.New("no")
	}
	cert, _, _ := NewCertificate([]string{"fleet-master.local"}, nil, time.Now())
	w, out := wizard(sys, "1", "", "fleet-master.local:8443", "y", "tess")
	w.Fetch = func(hp string) ([]byte, error) { return cert, nil }
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "isn't connected to a master Pi yet") {
		t.Errorf("output:\n%s", out)
	}
	for _, want := range []string{
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet activate -data /var/lib/pi-fleet -central https://fleet-master.local:8443 -ca /etc/pi-fleet/master.pem -username tess",
		"runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet sync -data /var/lib/pi-fleet",
	} {
		if !sys.ran(want) {
			t.Errorf("didn't run %q\n%s", want, strings.Join(sys.calls, "\n"))
		}
	}
	if sys.ran("set-master") || sys.ran("pi-fleet init") {
		t.Fatalf("ran:\n%s", strings.Join(sys.calls, "\n"))
	}
}

func TestReconnectAnEmployeePi(t *testing.T) {
	sys := newFake()
	sys.files[UnitPath] = []byte(NodeUnit())
	outputs(sys, map[string]string{"/opt/pi-fleet/current/pi-fleet version": "pi-fleet v1.0.0\n"})
	cert, _, _ := NewCertificate([]string{"pi-fleet.example.org"}, nil, time.Now())
	w, out := wizard(sys, "1", "pi-fleet.example.org", "y")
	w.Fetch = func(hp string) ([]byte, error) { return cert, nil }
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !sys.ran("runuser -u pifleet -- /opt/pi-fleet/current/pi-fleet set-master -data /var/lib/pi-fleet -url https://pi-fleet.example.org -ca /etc/pi-fleet/master.pem") ||
		!sys.ran("systemctl restart pi-fleet") || !bytes.Equal(sys.files[MasterCA], cert) {
		t.Fatalf("ran:\n%s", strings.Join(sys.calls, "\n"))
	}
}

// A drive someone mounted by hand (say at /mnt/ssd) is forgotten when
// setup erases it, so the Pi doesn't wait for it at boot.
func TestErasingADriveForgetsItsFstabLines(t *testing.T) {
	fstab := "PARTUUID=683deca6-02  /  ext4  defaults,noatime  0  1\n" +
		"UUID=83e91c32  /mnt/ssd  ext4  defaults,nofail  0  2\n" +
		"/dev/sdb2  /mnt/other  ext4  defaults  0  2\n" +
		"# UUID=83e91c32  /old  ext4  defaults  0  2\n"
	d := Disk{Path: "/dev/sdb", Parts: []Part{{Path: "/dev/sdb1", UUID: "83e91c32", Label: "ssd", Mounts: []string{"/mnt/ssd"}}}}
	want := "PARTUUID=683deca6-02  /  ext4  defaults,noatime  0  1\n" +
		"# drive erased by pi-fleet setup: UUID=83e91c32  /mnt/ssd  ext4  defaults,nofail  0  2\n" +
		"/dev/sdb2  /mnt/other  ext4  defaults  0  2\n" +
		"# UUID=83e91c32  /old  ext4  defaults  0  2\n"
	if got := forgetErased(fstab, d); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

// Choosing backups by mistake isn't a dead end: "skip" at the backup
// drive finishes setup without them.
func TestBackupsCanBeSkippedAtTheDrive(t *testing.T) {
	sys := newFake()
	outputs(sys, map[string]string{"lsblk": piDisks, "hostname": "raspberrypi\n", "blkid": "new-uuid\n", "id pifleet": "uid=999"})
	w, out := wizard(sys, "1", "", "", "2", "ERASE", "", "jsmith", "Jo Smith", "jo@example.org",
		"y",    // backups now
		"skip", // ...no, later
	)
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Backups aren't set up yet") || sys.ran("backup-config") {
		t.Fatalf("output:\n%s", out)
	}
}
