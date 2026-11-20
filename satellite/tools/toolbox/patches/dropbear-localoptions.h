/* localoptions.h for the Lenovo clock build of dropbear (copied into the source
 * tree by tools/toolbox/build.sh). Key-only root logins, our paths, no extras. */

/* No /etc/passwd on the clock: every getpwnam/getpwuid in dropbear goes through
 * these wrappers, which give root a built-in entry (home = our ssh dir, shell
 * /bin/sh). Root is the only account; logins are key-only. */
#define CLOCK_BUILTIN_ROOT "/data/local/clockd/ssh"
#include <pwd.h>
#include <string.h>
#include <sys/types.h>
static inline struct passwd *clock_root_pw(void) {
	static struct passwd r;
	r.pw_name = (char *)"root";
	r.pw_passwd = (char *)"!!";
	r.pw_uid = 0;
	r.pw_gid = 0;
	r.pw_gecos = (char *)"root";
	r.pw_dir = (char *)CLOCK_BUILTIN_ROOT;
	r.pw_shell = (char *)"/bin/sh";
	return &r;
}
static inline struct passwd *clock_getpwnam(const char *n) {
	struct passwd *p = getpwnam(n);
	return p ? p : (n && strcmp(n, "root") == 0 ? clock_root_pw() : NULL);
}
static inline struct passwd *clock_getpwuid(uid_t u) {
	struct passwd *p = getpwuid(u);
	return p ? p : (u == 0 ? clock_root_pw() : NULL);
}
#define getpwnam clock_getpwnam
#define getpwuid clock_getpwuid

/* keys only: no password logins at all, no PAM */
#define DROPBEAR_SVR_PASSWORD_AUTH 0
#define DROPBEAR_SVR_PAM_AUTH 0
#define DROPBEAR_CLI_PASSWORD_AUTH 0

/* nothing we need: forwarding of X11/agent, sftp (no sftp-server binary), motd */
#define DROPBEAR_X11FWD 0
#define DROPBEAR_SVR_AGENTFWD 0
#define DROPBEAR_CLI_AGENTFWD 0
#define DROPBEAR_SFTPSERVER 0
#define DO_MOTD 0

/* host keys live with clockd's state; ed25519 first, ecdsa as the fallback */
#define DSS_PRIV_FILENAME "/data/local/clockd/ssh/dropbear_dss_host_key"
#define RSA_PRIV_FILENAME "/data/local/clockd/ssh/dropbear_rsa_host_key"
#define ECDSA_PRIV_FILENAME "/data/local/clockd/ssh/dropbear_ecdsa_host_key"
#define ED25519_PRIV_FILENAME "/data/local/clockd/ssh/dropbear_ed25519_host_key"
#define DROPBEAR_RSA 0
#define DROPBEAR_DSS 0
#define DROPBEAR_PIDFILE "/tmp/dropbear.pid"

/* our tools first, then the vendor system */
#define DEFAULT_PATH "/data/local/clockd/tools/bin:/data/local/clockd/bin:/bin:/usr/bin:/sbin:/xbin"
#define DEFAULT_ROOT_PATH DEFAULT_PATH
