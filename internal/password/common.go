package password

import "strings"

// commonPasswords adds guessable passwords specific to pi-fleet's users
// (maintenance, calibration, Raspberry Pi) to the breached-password filter
// (breached.go), which catches the general ones.
var commonPasswords = map[string]bool{}

func init() {
	for _, p := range strings.Fields(`
		password1234 password12345 password123456 passw0rd1234 p@ssw0rd1234
		123456789012 1234567890123 12345678901234 qwertyuiop12 qwertyuiop123
		1q2w3e4r5t6y 1qaz2wsx3edc qazwsxedcrfv iloveyou1234 letmein12345
		welcome12345 welcome123456 changeme1234 changeme12345 administrator
		administrator1 trustno11234 football1234 baseball1234 monkey123456
		sunshine1234 princess1234 starwars1234 dragon123456 abcdefghijkl
		abcdefgh1234 abc123456789 aaaaaaaaaaaa 111111111111 000000000000
		passwordpassword correcthorsebatterystaple maintenance1 maintenance123
		calibration1 calibration123 equipment123 raspberrypi1 raspberrypi123
	`) {
		commonPasswords[p] = true
	}
}

func isCommon(lower string) bool { return commonPasswords[lower] }
