#!/usr/bin/env bash
# socmon diagnostics. Run as root on each board:
#   sudo bash socmon-diag.sh > "$(hostname)-diag.txt" 2>&1
# Read-only: it only lists and reads sysfs/procfs/debugfs files.

sec() { printf '\n===== %s =====\n' "$*"; }
show() { for f in "$@"; do [ -e "$f" ] && printf '%s: %s\n' "$f" "$(head -c 400 "$f" 2>&1 | tr '\0\n' ' ')"; done; }
drv() { basename "$(readlink "$1/driver" 2>/dev/null)" 2>/dev/null; }

sec system
uname -a
echo "model: $(tr -d '\0' </proc/device-tree/model 2>/dev/null)"
echo "compatible: $(tr '\0' ' ' </proc/device-tree/compatible 2>/dev/null)"
echo "euid: $(id -u)"
grep -iE 'Cma|MemTotal|MemAvailable' /proc/meminfo

sec cpufreq
ls -la /sys/devices/system/cpu/cpufreq/ 2>&1
for p in /sys/devices/system/cpu/cpufreq/policy*; do
  echo "-- $p"; show "$p"/{related_cpus,affected_cpus,scaling_driver,scaling_governor,scaling_cur_freq,cpuinfo_cur_freq,cpuinfo_min_freq,cpuinfo_max_freq,scaling_min_freq,scaling_max_freq,scaling_available_frequencies}
done
ls /sys/devices/system/cpu/cpu0/ /sys/devices/system/cpu/cpu0/cpufreq/ 2>&1
grep -E 'processor|CPU part' /proc/cpuinfo | paste - - 2>/dev/null
dmesg 2>/dev/null | grep -iE 'cpufreq|opp|dvfs' | tail -15

sec devfreq
for d in /sys/class/devfreq/*; do
  echo "-- $d -> $(readlink -f "$d")"
  show "$d"/{name,governor,cur_freq,min_freq,max_freq,available_frequencies,load,target_freq}
done

sec gpu
ls /sys/class/misc/ 2>&1
for g in /sys/devices/platform/*gpu* /sys/devices/platform/*/*gpu*; do
  [ -d "$g" ] || continue
  echo "-- $g driver=$(drv "$g")"
  show "$g"/{gpuinfo,core_mask,utilisation,gpuload,power/control,power/runtime_status,power/runtime_active_time,power/runtime_suspended_time}
  ls "$g" | tr '\n' ' '; echo
done
ls /sys/kernel/debug/mali0 2>&1 | tr '\n' ' '; echo
head -15 /sys/kernel/debug/mali0/gpu_memory 2>&1
ls /sys/kernel/debug/pvr 2>&1 | tr '\n' ' '; echo
head -30 /sys/kernel/debug/pvr/status 2>/dev/null
lsmod 2>/dev/null | grep -iE 'mali|bifrost|valhall|panfrost|panthor|pvr|rknpu|rocket|mpp|rga|vip|cedar|hantro|rkvdec|sunxi'
dmesg 2>/dev/null | grep -iE 'mali|panfrost|panthor|bifrost|pvr|gpu' | tail -20

sec npu
for f in /sys/kernel/debug/rknpu/*; do show "$f"; done
ls /sys/kernel/debug/viplite 2>&1 | tr '\n' ' '; echo
show /sys/kernel/debug/viplite/core_loading

sec media-devices
take() {
  for d in /sys/bus/platform/devices/*; do
    n=$(basename "$d")
    case "$n" in
      *rkvdec*|*rkvenc*|*vdpu*|*vepu*|*av1*|*jpeg*|*rga*|*iep*|*video-codec*|*cedar*|*.ve|*.ve[0-9]*|*mpp*|*vcodec*|*vdec*|*venc*)
        printf '%-28s drv=%-16s ctl=%-4s st=%-11s act=%-12s sus=%-12s' "$n" "$(drv "$d")" \
          "$(cat "$d/power/control" 2>/dev/null)" "$(cat "$d/power/runtime_status" 2>/dev/null)" \
          "$(cat "$d/power/runtime_active_time" 2>/dev/null)" "$(cat "$d/power/runtime_suspended_time" 2>/dev/null)"
        echo " pd=$(basename "$(readlink "$d/pm_domain" 2>/dev/null)" 2>/dev/null) usage=$(cat "$d/power/runtime_usage" 2>/dev/null) asd=$(cat "$d/power/autosuspend_delay_ms" 2>/dev/null)";;
    esac
  done
}
echo "--- sample 1"; take
sleep 3
echo "--- sample 2 (3 s later)"; take

sec mpp-procfs
ls -R /proc/mpp_service 2>&1 | head -80
for f in /proc/mpp_service/* /proc/mpp_service/*/*; do
  [ -f "$f" ] || continue
  echo "-- $f"; head -c 1500 "$f" 2>&1; echo
done
ls /sys/kernel/debug/ | tr '\n' ' '; echo
for f in /proc/rkrga/* /sys/kernel/debug/rkrga/*; do [ -f "$f" ] && { echo "-- $f"; head -30 "$f" 2>&1; }; done

sec device-nodes
ls -la /dev/mpp_service /dev/rga /dev/cedar_dev /dev/iep /dev/mali0 /dev/rknpu /dev/vipcore /dev/dri/ /dev/accel/ 2>&1
for v in /sys/class/video4linux/*; do echo "$(basename "$v"): $(cat "$v/name" 2>/dev/null) drv=$(basename "$(readlink "$v/device/driver")" 2>/dev/null)"; done
for c in /sys/class/drm/card* /sys/class/drm/renderD*; do [ -e "$c/device" ] && echo "$(basename "$c"): drv=$(basename "$(readlink "$c/device/driver")")"; done

sec hardware-users
for p in /proc/[0-9]*; do
  l=$(ls -l "$p/fd" 2>/dev/null | grep -oE '/dev/(mpp_service|rga|cedar_dev|iep|video[0-9]+|media[0-9]+|dri/[A-Za-z0-9]+|mali0|rknpu|vipcore|accel/[a-z0-9]+|dma_heap/[A-Za-z0-9_-]+)' | sort -u | tr '\n' ' ')
  [ -n "$l" ] && echo "$(basename "$p") [$(cat "$p/comm")] $(tr '\0' ' ' <"$p/cmdline" | cut -c1-140) :: $l"
done
echo "-- one DRM fdinfo sample:"
for p in /proc/[0-9]*; do
  for fd in "$p"/fd/*; do
    case "$(readlink "$fd" 2>/dev/null)" in /dev/dri/*) echo "$fd"; cat "$p/fdinfo/$(basename "$fd")"; break 2;; esac
  done
done

sec network
for i in /sys/class/net/*; do
  n=$(basename "$i")
  echo "$n type=$(cat "$i/type") oper=$(cat "$i/operstate") carrier=$(cat "$i/carrier" 2>/dev/null) speed=$(cat "$i/speed" 2>/dev/null) duplex=$(cat "$i/duplex" 2>/dev/null) master=$(basename "$(readlink "$i/master")" 2>/dev/null) drv=$(basename "$(readlink "$i/device/driver")" 2>/dev/null) phys=$([ -e "$i/device" ] && echo y) ports=$([ -d "$i/brif" ] && ls "$i/brif" | tr '\n' ',') wireless=$([ -d "$i/wireless" ] && echo y)"
done
cat /proc/net/wireless 2>/dev/null

sec thermal
for z in /sys/class/thermal/thermal_zone*; do echo "$(basename "$z") $(cat "$z/type") $(cat "$z/temp") policy=$(cat "$z/policy" 2>/dev/null)"; done
for c in /sys/class/thermal/cooling_device*; do echo "$(basename "$c") $(cat "$c/type") $(cat "$c/cur_state")/$(cat "$c/max_state")"; done
for h in /sys/class/hwmon/*; do echo "$(basename "$h") name=$(cat "$h/name") :: $(ls "$h" | grep -E 'temp|fan|pwm|in[0-9]|power|curr' | tr '\n' ' ')"; done

sec storage
lsblk -o NAME,TYPE,SIZE,MODEL,TRAN,FSTYPE,MOUNTPOINT 2>&1
grep -vE ' (proc|sysfs|cgroup2?|tmpfs|devtmpfs|devpts|mqueue|debugfs|tracefs|securityfs|pstore|bpf|configfs|fusectl|autofs|hugetlbfs|binfmt_misc|nsfs|overlay|rpc_pipefs|efivarfs|ramfs) ' /proc/mounts

sec socmon
for b in ./socmon /usr/local/bin/socmon ./socmon-linux-arm64; do
  [ -x "$b" ] && { "$b" -once | head -c 12000; break; }
done
