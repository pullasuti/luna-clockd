#!/bin/sh
# approve-login.sh - approve a waiting web UI login without touching the clock:
# injects a 1 s hold of the action button (KEY 139 on /dev/input/event0) over adb,
# which is what the physical button does while a login code is shown.
#
#   1. open the web UI in the new browser and click "Log in" (a code appears)
#   2. run: satellite/device/approve-login.sh [adb serial, default $ADB_SERIAL or the only device]
#   3. the browser logs in within a second or two
#
# The login waits 60 s. With no login waiting, the hold acts as a normal press of the
# action button (it would start a voice turn), so only run it while a code is shown.
S=${1:-$ADB_SERIAL}
A=${S:+-s $S}  # no serial: adb picks the only device
adb $A shell 'sendevent /dev/input/event0 1 139 1; sendevent /dev/input/event0 0 0 0; sleep 1; sendevent /dev/input/event0 1 139 0; sendevent /dev/input/event0 0 0 0' &&
	adb $A shell 'grep "login approved" /tmp/clockd.log | tail -1'
