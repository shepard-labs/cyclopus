#!/bin/sh
set -eu

domain=${1:-sslip.io}
expected=${2:-absent}
url=${PSL_URL:-https://publicsuffix.org/list/public_suffix_list.dat}

case "$expected" in present|absent) ;; *) printf '%s\n' "expected state must be present or absent" >&2; exit 64 ;; esac

tmp=$(mktemp "${TMPDIR:-/tmp}/cyclopus-psl.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM
curl -fsSL "$url" -o "$tmp" || { printf '%s\n' "BLOCKED: unable to fetch Public Suffix List" >&2; exit 2; }

if ruby -e 'd=ARGV[0].downcase; exit(File.readlines(ARGV[1], chomp: true).any? { |l| l.strip.downcase == d } ? 0 : 1)' "$domain" "$tmp"; then
  observed=present
else
  observed=absent
fi

printf '%s\n' "PSL probe: $domain is $observed"
test "$observed" = "$expected"
