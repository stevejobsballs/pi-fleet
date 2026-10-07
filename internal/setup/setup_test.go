package setup

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
		"1",          // master Pi
		"",           // name: fleet-master
		"",           // drive plugged in
		"1", "erase", // the USB stick, but not typed in capitals: back to the list
		"",           // drive plugged in
		"2", "ERASE", // the SSD
		"Jsmith", // not lowercase
		"jsmith", "Jo Smith", "jo at example", "jo@example.org",
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
	for _, want := range []string{"pi-fleet release keys", "release signing key", "may be a USB stick", "https://fleet-master.local", "Certificate fingerprint"} {
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
	w, out := wizard(sys, "1", "", "", "2", "y")
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
	w, out := wizard(sys, "1")
	w.Self = "/home/pi/Downloads/v1.0.0/pi-fleet_v1.0.0_linux_arm64"
	// Without the signature files next to it: nothing happens.
	if err := w.Run(); !errors.Is(err, ErrCancelled) || sys.ran("systemctl stop") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, f := range []string{"manifest.json", "manifest.json.minisig"} {
		sys.files["/home/pi/Downloads/v1.0.0/"+f] = []byte("x")
	}
	w, out = wizard(sys, "1")
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
	// Records on the SD card: only the move is offered.
	w, out := wizard(sys, "2")
	if err := w.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "Update to this version") || !strings.Contains(out.String(), "Move the records") {
		t.Fatalf("output:\n%s", out)
	}
	// On the drive already: nothing to do, nothing run.
	sys.mounts[DataDir] = true
	w, out = wizard(sys)
	if err := w.Run(); err != nil || !strings.Contains(out.String(), "Nothing to do") || sys.ran("systemctl") {
		t.Fatalf("err %v\n%s", err, out)
	}
}
