# decast.sh - runtime-only: stop Cast stack + audio helpers (a power cycle restores stock)
# run with: /sbin/busybox sh /data/local/tmp/decast.sh
stop process_monitor
stop cast_receiver
for p in process_manager; do kill $(pidof $p) 2>/dev/null; done
sleep 1
kill $(pidof cast_shell) $(pidof update_engine) $(pidof crash_uploader) 2>/dev/null
sleep 3
kill -9 $(pidof process_manager) $(pidof cast_shell) 2>/dev/null
stop sntpd   # toolbox applet, hardcoded time.google.com
stop lenovo_alarm   # drives the display (ttyS2/ttyS3), buttons, night light
stop panel_service  # writes the status LEDs (sn3112)
stop input_service  # reads the keypad
stop iot_audio
stop mute_service
sleep 1
echo "cast=[$(pidof cast_shell)] pm=[$(pidof process_manager)] iot=[$(pidof iot_audio_capture)] mute=[$(pidof mute_service)]"
