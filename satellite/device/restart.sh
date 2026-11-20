# restart.sh - install a pushed clockd.new (if any) and restart clockd.
# start.sh supervises clockd and starts it again within ~3 s.
#   adb push build/clockd-arm64 /data/local/clockd/clockd.new
#   adb shell /sbin/busybox sh /data/local/clockd/restart.sh   (toolbox sh has no "[")
F=/data/local/clockd
cd $F || exit 1
if [ -f clockd.new ]; then
    mv clockd.new clockd
    chmod 755 clockd
fi
# old busybox pgrep can't match it; ps shows the process as ./clockd
pids() { ps | while read -r u pid rest; do case "$rest" in *" ./clockd") echo "$pid" ;; esac; done; }
for p in $(pids); do
    kill "$p"
done
sleep 6
echo "clockd pid: $(pids)"
grep "clockd running" /tmp/clockd.log | tail -1 | cut -c1-100
