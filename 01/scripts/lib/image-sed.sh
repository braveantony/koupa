# Swap the koupa image in rendered manifests (sourced; no side effects).
# Needs KOUPA_REPO (images.env).

# ere_escape STRING: STRING with every ERE metacharacter (and the sed
# delimiter #) backslash-escaped, so it matches literally.
ere_escape() { printf '%s' "$1" | sed 's/[][\.^$*+?(){}|/#]/\\&/g'; }

# set_koupa_image IMAGE <in >out: every YAML `image:` whose complete value
# is KOUPA_REPO:vMAJOR[.MINOR[.PATCH]] becomes IMAGE. Neighbouring names
# (koupa-experiments, rush-hour), longer tags (v1.2.0-rc1) and the text
# elsewhere (env values, comments) are left alone. Fails when IMAGE has
# characters an image reference cannot have, or when no line was replaced
# (counted on the input: an IMAGE already present elsewhere does not count).
set_koupa_image() {
	local new=$1 re pat in n
	[[ $new =~ ^[A-Za-z0-9._/:@-]+$ ]] || { echo "set_koupa_image: bad image $new" >&2; return 1; }
	re=$(ere_escape "${KOUPA_REPO:?KOUPA_REPO is not set}")
	pat="^([[:space:]]*(- )?image:[[:space:]]+)\"?${re}:v[0-9]+(\\.[0-9]+){0,2}\"?[[:space:]]*\$"
	in=$(cat) || return 1
	n=$(grep -cE "$pat" <<<"$in") || n=0
	if [ "$n" -eq 0 ]; then
		printf '%s\n' "$in"
		echo "set_koupa_image: no image: $KOUPA_REPO:v... line to replace" >&2
		return 1
	fi
	sed -E "s#${pat}#\\1${new}#" <<<"$in"
}
