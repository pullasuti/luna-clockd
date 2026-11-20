# usage: aectest.sh <tag> <freq> <dbfs> [NOLB]
T=$1; F=$2; D=$3
if [ -n "$4" ]; then export NOLB=1; fi
/sbin/busybox nohup /data/local/tmp/mixsvc-arm64 tone $F $D 9 > /tmp/mix_$T.log 2>&1 &
sleep 0.3
/sbin/busybox nohup /data/local/tmp/capsvc-arm64 0 8 /tmp/${T}_raw.wav > /dev/null 2>&1 &
/data/local/tmp/capsvc-arm64 1 8 /tmp/${T}_aec.wav > /dev/null
sleep 2
grep -E "played|clients" /tmp/mix_$T.log
