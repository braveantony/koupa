# Registry helpers (sourced; no side effects on source, no traps).
#
# registry_digest REF ERRFILE: looks REF (REPO:TAG or REPO@sha256:...) up
# with `${SKOPEO:-skopeo} inspect` and prints
#   sha256:<64 hex>   it exists (the manifest digest)
#   absent            the registry answered "manifest unknown" for exactly
#                     this tag or digest of this repository (skopeo exit 2)
# and returns 0; on anything else (no network, unknown host, missing or bad
# credentials, 403, 5xx, a "not found" about something else, ...) it writes
# the reason to ERRFILE and returns 1. Absence is never assumed: callers
# that must not overwrite a tag fail closed on 1.
# Credentials come from the podman login (REGISTRY_AUTH_FILE or the default
# auth.json); nothing is passed on the command line.
registry_digest() {
	local ref=$1 errfile=$2 out rc=0 name part err
	out=$("${SKOPEO:-skopeo}" inspect --format '{{.Digest}}' "docker://$ref" 2>"$errfile") || rc=$?
	if [ "$rc" = 0 ]; then
		if [[ $out =~ ^sha256:[0-9a-f]{64}$ ]]; then
			echo "$out"
			return 0
		fi
		echo "unexpected digest for $ref: $out" >"$errfile"
		return 1
	fi
	case $ref in
	*@sha256:*) name=${ref%@*} part=${ref#*@} ;;
	*:*) name=${ref%:*} part=${ref##*:} ;;
	*) echo "registry_digest: $ref has no tag or digest" >"$errfile"; return 1 ;;
	esac
	err=$(cat "$errfile" 2>/dev/null)
	err=${err%$'\n'}
	local want="reading manifest $part in $name: manifest unknown"
	# skopeo exits 2 only when the image does not exist; the message must
	# name this very manifest (the line ends there, or the quoted msg does).
	if [ "$rc" = 2 ] && { [[ $err == *"$want\"" ]] || [[ $err == *"$want" ]]; }; then
		echo absent
		return 0
	fi
	{
		echo "cannot tell whether $ref exists (skopeo exit $rc): $(printf '%s' "$err" | head -c 300 | tr '\n' ' ')"
	} >"$errfile.msg"
	mv "$errfile.msg" "$errfile"
	return 1
}
