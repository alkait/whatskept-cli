# whatskept

## Dev

```
go build -o whatskept ./cmd/whatskept   # produces ./whatskept
codesign -f -s - whatskept   # needed on Go 1.22/macOS: fixes the cgo ad-hoc signature
./whatskept -h
./whatskept init [dir]   # initialize a workspace
# import (so far: validation + binding to device UDID and WhatsApp number)
WHATSKEPT_BACKUP_PASSWORD=... ./whatskept import <ios-backup-path>
./whatskept import --list   # list iOS backups on this machine
go test ./...            # run all tests
```
