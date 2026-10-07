# Breached-password filter

`breached.bloom` is a Bloom filter of passwords published from data
breaches. pi-fleet refuses any password it contains (case-insensitively), as
NIST SP 800-63B §5.1.1.2 recommends. It is built into the program, so the
check works offline on every Pi.

## Contents

About 2.2 million distinct passwords of 12 or more characters (shorter ones
are refused by the length rule anyway), lower-cased, from these lists in
[SecLists](https://github.com/danielmiessler/SecLists) at commit
`47cd752f4323f703e304104173633ee31462b9b3`:

| List | Lines | Kept | SHA-256 of the file |
|---|---|---|---|
| `Passwords/Common-Credentials/Pwdb_top-10000000.txt` | 10,000,000 | 722,208 | `18dc49ca32b62455a61e3398f4ab9f93eb700ff142fa0d4b9fd11a727f3b80e4` |
| `Passwords/Common-Credentials/100k-most-used-passwords-NCSC.txt` (UK NCSC, from Have I Been Pwned) | 99,840 | 1,212 | `c2e5696882c603b76bb67a47ee970897e5a76fc4c3f5547abe3d0ca340c576e0` |
| `Passwords/Leaked-Databases/rockyou.txt.tar.gz` (RockYou) | 14,344,391 | 1,568,422 | `47c070a029bcdb4cbd0e02c69fed136ef46dce4048ddbadf177daa5e885b8172` (archive) |

SecLists is MIT-licensed (Copyright (c) 2018 Daniel Miessler); see `NOTICE`
at the top of this repository. Only the filter is stored here, not the lists.

The filter has a false-positive rate of 1 in 1000: about one acceptable
password in a thousand is refused as breached, and the person chooses
another. It can't be turned back into the list.

## Rebuilding

```sh
B=https://raw.githubusercontent.com/danielmiessler/SecLists/47cd752f4323f703e304104173633ee31462b9b3
curl -fLO $B/Passwords/Common-Credentials/Pwdb_top-10000000.txt
curl -fLO $B/Passwords/Common-Credentials/100k-most-used-passwords-NCSC.txt
curl -fLO $B/Passwords/Leaked-Databases/rockyou.txt.tar.gz && tar -xzf rockyou.txt.tar.gz
go run ./internal/password/genbloom -out internal/password/breached.bloom \
    Pwdb_top-10000000.txt 100k-most-used-passwords-NCSC.txt rockyou.txt
```

The same lists always give the same file. To add lists, append them to the
command and update the table above.
