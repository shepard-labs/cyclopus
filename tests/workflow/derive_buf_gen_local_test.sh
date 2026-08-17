#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
DERIVER="$ROOT/scripts/derive_buf_gen_local.rb"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cyclopus-buf-template-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
assertions=0

fail() { printf '%s\n' "not ok - $1" >&2; exit 1; }
pass() { assertions=$((assertions + 1)); printf '%s\n' "ok - $1"; }

derive() {
  ruby "$DERIVER" "$1" "$tmp/local.yaml" "$tmp/installs"
}

mutate() {
  cp "$ROOT/buf.gen.yaml" "$tmp/input.yaml"
  ruby -ryaml -e "$1" "$tmp/input.yaml"
}

assert_derived() {
  name=$1
  expression=$2
  ruby -ryaml -e "$expression" "$tmp/local.yaml" "$tmp/installs" || fail "$name"
  pass "$name"
}

assert_rejected() {
  name=$1
  expression=$2
  mutate "$expression"
  if derive "$tmp/input.yaml" >/dev/null 2>&1; then
    fail "$name was accepted"
  fi
  pass "$name is rejected"
}

assert_raw_rejected() {
  name=$1
  contents=$2
  printf '%s' "$contents" >"$tmp/input.yaml"
  if derive "$tmp/input.yaml" >/dev/null 2>&1; then
    fail "$name was accepted"
  fi
  pass "$name is rejected"
}

derive "$ROOT/buf.gen.yaml" || fail "canonical template derivation failed"
assert_derived "canonical plugin metadata is derived" '
  c=YAML.safe_load_file(ARGV[0], aliases: false)
  expected=[
    ["protoc-gen-go", "gen/go", ["paths=source_relative"]],
    ["protoc-gen-go-grpc", "gen/go", ["paths=source_relative"]],
    ["protoc-gen-grpc-gateway", "gen/go", ["paths=source_relative"]],
    ["protoc-gen-openapiv2", "gen/openapi", ["allow_repeated_fields_in_body=true"]]
  ]
  actual=c.fetch("plugins").map { |p| [p.fetch("local"), p.fetch("out"), p.fetch("opt")] }
  abort unless c["version"] == "v2" && c["clean"] == true && actual == expected
  installs=File.readlines(ARGV[1], chomp: true)
  abort unless installs == [
    "google.golang.org/protobuf/cmd/protoc-gen-go\tv1.36.11",
    "google.golang.org/grpc/cmd/protoc-gen-go-grpc\tv1.6.2",
    "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway\tv2.29.0",
    "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2\tv2.29.0"
  ]'

mutate 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["remote"]="buf.build/protocolbuffers/go:9.8.7"; File.write(p,YAML.dump(d))'
derive "$tmp/input.yaml" || fail "version mutation was rejected"
assert_derived "remote version mutation changes install version" 'abort unless File.readlines(ARGV[1], chomp: true).first.end_with?("\tv9.8.7")'

mutate 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["out"]="different/go"; File.write(p,YAML.dump(d))'
derive "$tmp/input.yaml" || fail "out mutation was rejected"
assert_derived "remote out mutation changes local out" 'abort unless YAML.safe_load_file(ARGV[0], aliases: false)["plugins"][0]["out"] == "different/go"'

mutate 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["opt"]=["paths=import"]; File.write(p,YAML.dump(d))'
derive "$tmp/input.yaml" || fail "opt mutation was rejected"
assert_derived "remote opt mutation changes local opt" 'abort unless YAML.safe_load_file(ARGV[0], aliases: false)["plugins"][0]["opt"] == ["paths=import"]'

mutate 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"].rotate!; File.write(p,YAML.dump(d))'
derive "$tmp/input.yaml" || fail "order mutation was rejected"
assert_derived "remote order mutation changes local and install order" '
  c=YAML.safe_load_file(ARGV[0], aliases: false)
  abort unless c["plugins"].map { |p| p["local"] } == %w[protoc-gen-go-grpc protoc-gen-grpc-gateway protoc-gen-openapiv2 protoc-gen-go]
  abort unless File.readlines(ARGV[1], chomp: true).first.start_with?("google.golang.org/grpc/cmd/protoc-gen-go-grpc\t")'

mutate 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["remote"]="buf.build/grpc/go:v1.6.2"; File.write(p,YAML.dump(d))'
if derive "$tmp/input.yaml" >/dev/null 2>&1; then fail "known plugin identity mutation created a duplicate and was accepted"; fi
pass "known plugin identity mutation is validated"

assert_rejected "unknown plugin" 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["remote"]="buf.build/example/unknown:v1.0.0"; File.write(p,YAML.dump(d))'
assert_rejected "missing version" 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["remote"]="buf.build/protocolbuffers/go"; File.write(p,YAML.dump(d))'
assert_rejected "duplicate plugin" 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"] << d["plugins"][0].dup; File.write(p,YAML.dump(d))'
assert_rejected "missing plugin identity" 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0].delete("remote"); File.write(p,YAML.dump(d))'
assert_rejected "unsupported plugin field" 'p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d["plugins"][0]["types"]=["example.Type"]; File.write(p,YAML.dump(d))'

for version in 'vv1.2.3' '1.2' '01.2.3' '1.02.3' '1.2.03' '1.2.3-' '1.2.3+bad!' '1.2.3/evil'; do
  assert_rejected "malformed version $version" "p=ARGV[0]; d=YAML.safe_load_file(p, aliases: false); d[\"plugins\"][0][\"remote\"]=\"buf.build/protocolbuffers/go:$version\"; File.write(p,YAML.dump(d))"
done
assert_raw_rejected "version containing newline" 'version: v2
clean: true
plugins:
  - remote: "buf.build/protocolbuffers/go:v1.2.3\nmalicious"
    out: gen/go
'
assert_raw_rejected "version containing tab" 'version: v2
clean: true
plugins:
  - remote: "buf.build/protocolbuffers/go:v1.2.3\tmalicious"
    out: gen/go
'

assert_raw_rejected "duplicate top-level key" 'version: v2
version: v2
clean: true
plugins: []
'
for field in remote out opt strategy include_imports include_wkt; do
  assert_raw_rejected "duplicate plugin $field key" "version: v2
clean: true
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen/go
    $field: duplicate
    $field: duplicate
"
done
assert_raw_rejected "duplicate nested mapping key in sequence" 'version: v2
clean: true
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen/go
    unsupported:
      - nested: first
        nested: second
'

mkdir "$tmp/fake-bin"
cat >"$tmp/fake-bin/go" <<'EOF'
#!/bin/sh
case " $* " in
  *' config migrate '*) test "$FAKE_MODE" = template_status2 && exit 2; exit 0 ;;
  *' generate --template '*)
    case "$FAKE_MODE" in transient|resource_exhausted|deadline_exceeded|status429) ;; *) exit 90 ;; esac
    mkdir -p gen internal/apidocs
    cp -R "$SOURCE_ROOT/gen/go" gen/go
    cp -R "$SOURCE_ROOT/gen/openapi" gen/openapi
    : >"$FALLBACK_MARKER"
    exit 0
    ;;
  *' generate '*)
    if test "$FAKE_MODE" = transient; then
      printf '%s\n' 'rpc error: code = Unavailable desc = transport failure' >&2
      exit 8
    fi
    if test "$FAKE_MODE" = status429; then
      printf '%s\n' 'plugin download failed: HTTP status 429 TooManyRequests' >&2
      exit 11
    fi
    if test "$FAKE_MODE" = proto429; then
      printf '%s\n' 'api/cyclopus/v1/cyclopus.proto:429:7: syntax error' >&2
      exit 10
    fi
    if test "$FAKE_MODE" = buf_status2; then
      printf '%s\n' 'unknown plugin buf.build/example/missing' >&2
      exit 2
    fi
    if test "$FAKE_MODE" = standalone_token; then
      printf '%s\n' "$STANDALONE_TOKEN" >&2
      exit 12
    fi
    if test "$FAKE_MODE" = mixed_diagnostic; then
      printf '%s\n' 'rpc error: code = Unavailable desc = transport failure' >&2
      printf '%s\n' 'api/cyclopus/v1/cyclopus.proto:27:4: syntax error' >&2
      exit 13
    fi
    if test "$FAKE_MODE" = resource_exhausted; then
      printf '%s\n' 'rpc error: code = ResourceExhausted desc = quota exhausted' >&2
      exit 14
    fi
    if test "$FAKE_MODE" = deadline_exceeded; then
      printf '%s\n' 'rpc error: code = DeadlineExceeded desc = transport timeout' >&2
      exit 15
    fi
    printf '%s\n' 'api/cyclopus/v1/cyclopus.proto:1:1: syntax error' >&2
    exit 9
    ;;
  *' ./cmd/gen-openapi '*) cp "$SOURCE_ROOT/internal/apidocs/openapi.json" internal/apidocs/openapi.json; exit 0 ;;
  *' install '*) : >"$FALLBACK_MARKER"; exit 0 ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$tmp/fake-bin/go"
cat >"$tmp/fake-bin/ruby" <<'EOF'
#!/bin/sh
if test "$FAKE_MODE" = deriver_status2 && test "$1" = "$SOURCE_ROOT/scripts/derive_buf_gen_local.rb"; then
  exit 2
fi
exec "$REAL_RUBY" "$@"
EOF
chmod +x "$tmp/fake-bin/ruby"
REAL_RUBY=$(command -v ruby)

assert_no_fallback_status() {
  name=$1
  mode=$2
  expected_status=$3
  shift 3
  rm -f "$tmp/fallback-ran"
  set +e
  PATH="$tmp/fake-bin:$PATH" SOURCE_ROOT="$ROOT" REAL_RUBY="$REAL_RUBY" FAKE_MODE="$mode" FALLBACK_MARKER="$tmp/fallback-ran" VERIFY_GENERATION_REMOTE_ATTEMPTS=1 "$@" sh "$ROOT/scripts/verify_generation.sh" >"$tmp/$mode.log" 2>&1
  status=$?
  set -e
  if test "$status" -ne "$expected_status"; then
    cat "$tmp/$mode.log" >&2
    fail "$name returned $status instead of $expected_status"
  fi
  test ! -e "$tmp/fallback-ran" || fail "$name ran local fallback"
  pass "$name does not fallback and preserves status"
}

assert_fallback() {
  name=$1
  mode=$2
  rm -f "$tmp/fallback-ran"
  PATH="$tmp/fake-bin:$PATH" SOURCE_ROOT="$ROOT" REAL_RUBY="$REAL_RUBY" FAKE_MODE="$mode" FALLBACK_MARKER="$tmp/fallback-ran" VERIFY_GENERATION_REMOTE_ATTEMPTS=1 sh "$ROOT/scripts/verify_generation.sh" >"$tmp/$mode.log" 2>&1 || {
    cat "$tmp/$mode.log" >&2
    fail "$name did not complete through fallback"
  }
  test -e "$tmp/fallback-ran" || fail "$name did not run fallback"
  pass "$name is transient"
}

assert_no_fallback_status "invalid remote template status 2" template_status2 2 env
assert_no_fallback_status "local derivation status 2" deriver_status2 2 env
assert_no_fallback_status "non-transient Buf status 2" buf_status2 2 env

set +e
PATH="$tmp/fake-bin:$PATH" SOURCE_ROOT="$ROOT" REAL_RUBY="$REAL_RUBY" FAKE_MODE=proto FALLBACK_MARKER="$tmp/fallback-ran" VERIFY_GENERATION_REMOTE_ATTEMPTS=1 sh "$ROOT/scripts/verify_generation.sh" >"$tmp/non-transient.log" 2>&1
status=$?
set -e
if test "$status" -ne 9; then
  cat "$tmp/non-transient.log" >&2
  fail "non-transient proto error returned $status instead of 9"
fi
test ! -e "$tmp/fallback-ran" || fail "non-transient proto error ran local fallback"
pass "non-transient proto error does not fallback"

rm -f "$tmp/fallback-ran"
set +e
PATH="$tmp/fake-bin:$PATH" SOURCE_ROOT="$ROOT" REAL_RUBY="$REAL_RUBY" FAKE_MODE=proto429 FALLBACK_MARKER="$tmp/fallback-ran" VERIFY_GENERATION_REMOTE_ATTEMPTS=1 sh "$ROOT/scripts/verify_generation.sh" >"$tmp/proto-429.log" 2>&1
status=$?
set -e
if test "$status" -ne 10; then
  cat "$tmp/proto-429.log" >&2
  fail "proto line 429 error returned $status instead of 10"
fi
test ! -e "$tmp/fallback-ran" || fail "proto line 429 error ran local fallback"
pass "proto line 429 diagnostic is non-transient"

for token in TooManyRequests 'too many requests' 'rate limit'; do
  assert_no_fallback_status "standalone $token" standalone_token 12 env STANDALONE_TOKEN="$token"
done

assert_no_fallback_status "mixed transport and proto diagnostic" mixed_diagnostic 13 env

assert_fallback "structured Unavailable RPC error" transient
assert_fallback "structured ResourceExhausted RPC error" resource_exhausted
assert_fallback "structured DeadlineExceeded RPC error" deadline_exceeded
assert_fallback "explicit plugin-download HTTP status 429" status429

printf '%s\n' "Buf local template derivation tests passed: $assertions assertions"
