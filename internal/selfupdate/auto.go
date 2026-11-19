package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Auto-update is gpu-metrics-exporter-update.timer, which runs `update -auto`
// through gpu-metrics-exporter-update.service. The timer's enablement is the
// setting itself: the package enables it once, when it installs the timer for
// the first time, and leaves it as it finds it on every upgrade after that --
// so `systemctl disable` on the timer means exactly what `auto-update disable`
// does, and both last.

// SetAutoUpdate turns auto-update on or off: it enables and starts the timer,
// or disables and stops it, then reports the new state.
func (u *Updater) SetAutoUpdate(enable bool) error {
	if u.inKubernetes() {
		return errKubernetes
	}
	verb := "disable"
	if enable {
		verb = "enable"
	}
	if err := u.checkHost("gpu-metrics-exporter auto-update " + verb); err != nil {
		return err
	}
	timer, err := u.unitState(timerName)
	if err != nil {
		return err
	}
	if timer.LoadState != "loaded" {
		return fmt.Errorf("%s is %s: the installed package predates auto-update; upgrade it first", timerName, timer.LoadState)
	}
	if err := u.systemctl(verb, "--now", timerName); err != nil {
		return err
	}
	return u.AutoUpdateStatus()
}

// AutoUpdateStatus reports whether auto-update is on, when it checks next,
// and how its last check went. It needs no root.
func (u *Updater) AutoUpdateStatus() error {
	if u.inKubernetes() {
		return errKubernetes
	}
	timer, err := u.unitState(timerName)
	if err != nil {
		if u.geteuid() != 0 {
			// Root's systemctl talks to systemd directly; anyone else's
			// goes through D-Bus, which a minimal system may lack.
			return fmt.Errorf("%w; without root, systemctl needs D-Bus: try it with sudo", err)
		}
		return err
	}
	switch {
	case timer.LoadState != "loaded":
		u.printf("auto-update: not installed (%s is %s); the installed package predates it\n", timerName, timer.LoadState)
		return nil
	case timer.UnitFileState == "enabled":
		u.printf("auto-update: enabled\n")
	default:
		u.printf("auto-update: disabled\n")
	}
	// The timer's own table: when it fires next and when it last did.
	if table, err := u.cmd.output("systemctl", "list-timers", "--all", "--no-pager", timerName); err == nil {
		for _, line := range strings.Split(table, "\n") {
			if line = strings.TrimRight(line, " "); line != "" && !strings.HasSuffix(line, "timers listed.") {
				u.printf("  %s\n", line)
			}
		}
	}
	last, err := u.show(updateServiceName, "Result", "ExecMainStatus", "ExecMainExitTimestamp")
	if err != nil {
		return err
	}
	u.printf("last check: %s\n", describeLastCheck(last))
	u.printf("its log: journalctl -u %s\n", updateServiceName)
	return nil
}

// describeLastCheck reads how the update unit's last run ended. systemd keeps
// that only until the next reboot.
func describeLastCheck(p map[string]string) string {
	at := p["ExecMainExitTimestamp"]
	if at == "" || at == "n/a" {
		return "none since boot"
	}
	switch {
	case p["Result"] == "success" && p["ExecMainStatus"] == "0":
		return at + ": completed"
	case p["Result"] == "success" && p["ExecMainStatus"] == strconv.Itoa(ExitTempFail):
		return at + ": could not finish, and changed nothing; the next check tries again"
	default:
		return fmt.Sprintf("%s: failed (%s, exit status %s)", at, p["Result"], p["ExecMainStatus"])
	}
}
