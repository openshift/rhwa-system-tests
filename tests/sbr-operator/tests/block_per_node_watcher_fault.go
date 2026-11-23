package tests

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
)

type bpnWatcherReadFault struct {
	device *bpnProbeDevice
	owner  string
}

const bpnWatcherFaultScope = `
control=/sys/kernel/debug/fail_make_request
lock=/run/sbr-bpn-watcher-fault
state="$lock.$owner"
verify_disk() {
    test "$(cat "$disk/diskseq")" = "$diskseq" || {
        echo "The RBD disk identity changed" >&2
        exit 1
    }
}
require_idle() {
    verify_disk
    test -w "$disk/make-it-fail" -a -w "$control/probability" -a -w "$control/times" -a -w "$control/verbose" || {
        echo "The watcher kernel must expose writable CONFIG_FAIL_MAKE_REQUEST debugfs and sysfs controls" >&2
        exit 1
    }
    test "$(cat "$control/probability")" = 0 -a "$(cat "$control/interval")" = 1 \
        -a "$(cat "$control/space")" = 0 -a "$(cat "$control/task-filter")" = N || {
        echo "The host has an active or nondefault block fault configuration" >&2
        exit 1
    }
    require_device_flags 0
}
require_device_flags() {
    for flag in /sys/dev/block/*/make-it-fail; do
        expected=0
        if test "$flag" = "$disk/make-it-fail"; then
            expected=$1
        fi
        test "$(cat "$flag")" = "$expected" || {
            echo "Unexpected block fault flag $flag" >&2
            exit 1
        }
    done
}
`

func (fault *bpnWatcherReadFault) command(ctx context.Context, script string) (string, error) {
	return bpnWatcherHostCommand(ctx, fault.device.host, "owner="+bpnQuote(fault.owner)+"\n"+
		fault.device.diskVariables()+bpnWatcherFaultScope+script)
}

func (device *bpnProbeDevice) diskVariables() string {
	return "disk=" + bpnQuote(device.disk) + "\ndiskseq=" + bpnQuote(device.diskSeq) + "\n"
}

func (fault *bpnWatcherReadFault) prepare(ctx context.Context) error {
	_, err := fault.command(ctx, `
test ! -e "$lock" || { echo "An earlier watcher fault owns $lock" >&2; exit 1; }
require_idle
`)

	return err
}

func (fault *bpnWatcherReadFault) activate(ctx context.Context) error {
	_, err := fault.command(ctx, `
require_idle
umask 077
trap 'rm -f "$state"' EXIT
trap 'exit 1' HUP INT TERM
(
    set -C
    { printf '%s\n' "$owner"; cat "$control/times" "$control/verbose"; } > "$state"
)
# Publish the original settings and owner in one operation before any kernel write.
ln -T "$state" "$lock"
if ! (require_idle); then
    rm "$lock"
    exit 1
fi
exec 9>"$disk/make-it-fail"
verify_disk
printf '%s\n' -1 > "$control/times"
printf '%s\n' 0 > "$control/verbose"
printf '%s\n' 1 >&9
verify_disk
require_device_flags 1
printf '%s\n' 100 > "$control/probability"
`)

	return err
}

func (fault *bpnWatcherReadFault) confirm(ctx context.Context) error {
	_, err := fault.command(ctx, `
verify_disk
test "$(head -n 1 "$lock")" = "$owner"
test "$(cat "$control/probability")" = 100 -a "$(cat "$control/times")" = -1
require_device_flags 1
`+bpnWatcherDirectRead+`
test "$result" -eq 1
case "$output" in
    *"error reading"*"Input/output error"*) ;;
    *) printf '%s\n' "$output" >&2; exit 1 ;;
esac
`)

	return err
}

func (fault *bpnWatcherReadFault) restore(ctx context.Context) error {
	_, err := fault.command(ctx, `
if ! test -e "$lock"; then
    rm -f "$state"
    exit 0
fi
set -- $(cat "$lock")
test "$#" -eq 3
test "$1" = "$owner" || { echo "Another run owns the block fault controls" >&2; exit 1; }
shift
printf '%s\n' 0 > "$control/probability"
if test -e "$disk/diskseq" && test "$(cat "$disk/diskseq")" = "$diskseq"; then
    exec 9>"$disk/make-it-fail"
    verify_disk
    printf '%s\n' 0 >&9
    test "$(cat "$disk/make-it-fail")" = 0
fi
printf '%s\n' "$1" > "$control/times"
printf '%s\n' "$2" > "$control/verbose"
test "$(cat "$control/probability")" = 0 -a "$(cat "$control/times")" = "$1" \
    -a "$(cat "$control/verbose")" = "$2"
rm -f "$state"
rm "$lock"
`)

	return err
}

const bpnWatcherDirectRead = `
test "$(cat "$disk/diskseq")" = "$diskseq"
dev="/dev/$(basename "$(readlink -f "$disk")")"
test -b "$dev"
test "$(cat "$disk/size")" -ge 8
pair=$(stat -Lc '%t %T' "$dev")
set -- $pair
test "$disk" = "/sys/dev/block/$((0x$1)):$((0x$2))"
result=0
output=$(LC_ALL=C timeout 5 dd if="$dev" of=/dev/null bs=4096 count=1 \
    iflag=direct,fullblock status=none 2>&1) || result=$?
`

func (device *bpnProbeDevice) directRead(ctx context.Context) error {
	_, err := bpnWatcherHostCommand(ctx, device.host, device.diskVariables()+bpnWatcherDirectRead+`
if test "$result" -ne 0; then
    printf '%s\n' "$output" >&2
    exit 1
fi
`)

	return err
}

func (device *bpnProbeDevice) completedWrites(ctx context.Context) (uint64, error) {
	output, err := bpnWatcherHostCommand(ctx, device.host, device.diskVariables()+`
test "$(cat "$disk/diskseq")" = "$diskseq"
awk '{print $5}' "$disk/stat"
`)
	if err != nil {
		return 0, err
	}

	return strconv.ParseUint(strings.TrimSpace(output), 10, 64)
}

func (device *bpnProbeDevice) probeAPI(ctx context.Context, address string, port int32) error {
	connection := "exec 3<>/dev/tcp/" + address + "/" + strconv.Itoa(int(port))
	command := "sudo nsenter --target " + strconv.Itoa(device.pid) +
		" --net -- timeout 3 bash -c " + bpnQuote(connection)
	if _, err := device.host.Run(ctx, sbrparams.BPNHostCall, command); err != nil {
		return fmt.Errorf("API TCP access from pod %s: %w", device.pod.Name, err)
	}

	return nil
}
