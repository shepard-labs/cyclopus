#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cyclopus-real-fallback-test.XXXXXX")
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$tmp/bin"
real_go=$(command -v go)

cat >"$tmp/timeout.rb" <<'EOF'
timeout=Float(ARGV.shift)
grace=Float(ARGV.shift)
clock=Process::CLOCK_MONOTONIC
signal_status=nil
{"HUP" => 129, "INT" => 130, "TERM" => 143}.each do |signal, status|
  Signal.trap(signal) { signal_status ||= status }
end
process=spawn(*ARGV, pgroup: true)

leader_status=nil
reap_leader = lambda do
  unless leader_status
    begin
      result=Process.waitpid2(process, Process::WNOHANG)
      leader_status=result[1] if result
    rescue Errno::ECHILD
    end
  end
  leader_status
end

group_exists = lambda do
  begin
    Process.kill(0, -process)
    true
  rescue Errno::ESRCH
    false
  rescue Errno::EPERM
    true
  end
end

cleanup_group = lambda do
  begin
    Process.kill("TERM", -process)
  rescue Errno::ESRCH
  end
  deadline=Process.clock_gettime(clock) + grace
  while group_exists.call && Process.clock_gettime(clock) < deadline
    reap_leader.call
    sleep 0.02
  end
  begin
    Process.kill("KILL", -process)
  rescue Errno::ESRCH
  end
  deadline=Process.clock_gettime(clock) + grace
  while Process.clock_gettime(clock) < deadline
    reap_leader.call
    break unless group_exists.call
    sleep 0.02
  end
  reap_leader.call
end

deadline=Process.clock_gettime(clock) + timeout
loop do
  status=reap_leader.call
  if signal_status
    cleanup_group.call
    exit signal_status
  end
  if status
    cleanup_group.call if status.exited? && group_exists.call
    exit(status.exited? ? status.exitstatus : 128 + status.termsig)
  end
  if Process.clock_gettime(clock) >= deadline
    cleanup_group.call
    exit 124
  end
  sleep 0.02
end
EOF

cat >"$tmp/ignore-term.sh" <<'EOF'
#!/bin/sh
trap '' TERM
printf '%s\n' "$$" >"$1"
while :; do sleep 1; done
EOF
chmod +x "$tmp/ignore-term.sh"

cat >"$tmp/signal-supervisor-test.rb" <<'EOF'
signal, expected, timeout_script, child_script, pid_file=ARGV
supervisor=nil
group=nil
begin
  supervisor=spawn("ruby", timeout_script, "30", "0.15", "sh", child_script, pid_file)
  deadline=Process.clock_gettime(Process::CLOCK_MONOTONIC) + 1
  until File.exist?(pid_file)
    abort "child did not start" if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
    sleep 0.01
  end
  group=Integer(File.read(pid_file))
  Process.kill(signal, supervisor)
  status=nil
  deadline=Process.clock_gettime(Process::CLOCK_MONOTONIC) + 2
  until status
    result=Process.waitpid2(supervisor, Process::WNOHANG)
    status=result[1] if result
    abort "supervisor did not exit after #{signal}" if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
    sleep 0.01 unless status
  end
  abort "#{signal} status was #{status.exitstatus}" unless status.exitstatus == Integer(expected)
  deadline=Process.clock_gettime(Process::CLOCK_MONOTONIC) + 1
  loop do
    begin
      Process.kill(0, -group)
      abort "#{signal} process group survived" if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
      sleep 0.01
    rescue Errno::ESRCH
      break
    end
  end
ensure
  begin Process.kill("KILL", -group) if group rescue Errno::ESRCH end
  begin Process.kill("KILL", supervisor) if supervisor rescue Errno::ESRCH end
  begin Process.waitpid(supervisor) if supervisor rescue Errno::ECHILD end
end
EOF

for signal_status in HUP:129 INT:130 TERM:143; do
  signal=${signal_status%:*}
  expected=${signal_status#*:}
  rm -f "$tmp/signal-child.pid"
  ruby "$tmp/signal-supervisor-test.rb" "$signal" "$expected" "$tmp/timeout.rb" "$tmp/ignore-term.sh" "$tmp/signal-child.pid" >/dev/null 2>&1
done

set +e
ruby -e '
  output=ARGV.shift
  started=Process.clock_gettime(Process::CLOCK_MONOTONIC)
  system(*ARGV)
  elapsed=Process.clock_gettime(Process::CLOCK_MONOTONIC)-started
  File.write(output, "#{$?.exitstatus} #{elapsed}\n")
' "$tmp/timeout-result" ruby "$tmp/timeout.rb" 0.2 0.2 sh "$tmp/ignore-term.sh" "$tmp/ignore-term.pid" >/dev/null 2>&1
timeout_runner_status=$?
set -e
test "$timeout_runner_status" -eq 0
read -r timeout_status timeout_elapsed <"$tmp/timeout-result"
test "$timeout_status" -eq 124
ruby -e 'abort unless Float(ARGV[0]) < 2' "$timeout_elapsed"
timeout_child=$(cat "$tmp/ignore-term.pid")
ruby -e '
  begin
    Process.kill(0, -Integer(ARGV[0]))
    abort "timeout process group survived KILL"
  rescue Errno::ESRCH
  end
' "$timeout_child"

cat >"$tmp/leader-exits-with-descendant.sh" <<'EOF'
#!/bin/sh
trap '' TERM
sh -c 'trap "" TERM; while :; do sleep 1; done' &
printf '%s\n' "$$" >"$1"
exit 7
EOF
chmod +x "$tmp/leader-exits-with-descendant.sh"

set +e
ruby -e '
  output=ARGV.shift
  started=Process.clock_gettime(Process::CLOCK_MONOTONIC)
  system(*ARGV)
  elapsed=Process.clock_gettime(Process::CLOCK_MONOTONIC)-started
  File.write(output, "#{$?.exitstatus} #{elapsed}\n")
' "$tmp/leader-exit-result" ruby "$tmp/timeout.rb" 5 0.15 sh "$tmp/leader-exits-with-descendant.sh" "$tmp/exited-leader.pid" >/dev/null 2>&1
leader_exit_runner_status=$?
set -e
test "$leader_exit_runner_status" -eq 0
read -r leader_exit_status leader_exit_elapsed <"$tmp/leader-exit-result"
test "$leader_exit_status" -eq 7
ruby -e 'abort unless Float(ARGV[0]) < 2' "$leader_exit_elapsed"
exited_leader=$(cat "$tmp/exited-leader.pid")
ruby -e '
  begin
    Process.kill(0, -Integer(ARGV[0]))
    abort "descendant survived cleanup after leader exit"
  rescue Errno::ESRCH
  end
' "$exited_leader"

cat >"$tmp/leader-exits-term.sh" <<'EOF'
#!/bin/sh
trap 'printf "%s\n" term >"$3"; exit 0' TERM
trap '' INT HUP
sh -c 'trap "" TERM; while :; do sleep 1; done' &
printf '%s\n' "$$" >"$1"
printf '%s\n' "$!" >"$2"
while :; do sleep 1; done
EOF
chmod +x "$tmp/leader-exits-term.sh"

set +e
ruby -e '
  output=ARGV.shift
  started=Process.clock_gettime(Process::CLOCK_MONOTONIC)
  system(*ARGV)
  elapsed=Process.clock_gettime(Process::CLOCK_MONOTONIC)-started
  File.write(output, "#{$?.exitstatus} #{elapsed}\n")
' "$tmp/descendant-result" ruby "$tmp/timeout.rb" 0.2 0.15 sh "$tmp/leader-exits-term.sh" "$tmp/leader.pid" "$tmp/descendant.pid" "$tmp/leader-term" >/dev/null 2>&1
descendant_runner_status=$?
set -e
test "$descendant_runner_status" -eq 0
read -r descendant_status descendant_elapsed <"$tmp/descendant-result"
test "$descendant_status" -eq 124
ruby -e 'abort unless Float(ARGV[0]) < 2' "$descendant_elapsed"
test -e "$tmp/leader-term"
leader=$(cat "$tmp/leader.pid")
ruby -e '
  begin
    Process.kill(0, -Integer(ARGV[0]))
    abort "TERM-resistant descendant process group survived KILL"
  rescue Errno::ESRCH
  end
' "$leader"

cat >"$tmp/bin/go" <<'EOF'
#!/bin/sh
if test "$#" -eq 2 && test "$1" = install; then
  printf '%s\n' "$2" >>"$INSTALL_LOG"
elif test "$#" -eq 3 &&
  test "$1" = run &&
  test "$2" = github.com/bufbuild/buf/cmd/buf@v1.57.0 &&
  test "$3" = generate; then
  : >"$REMOTE_OUTAGE_MARKER"
  printf '%s\n' 'rpc error: code = Unavailable desc = simulated remote plugin outage' >&2
  exit 8
elif test "$#" -eq 5 &&
  test "$1" = run &&
  test "$2" = github.com/bufbuild/buf/cmd/buf@v1.57.0 &&
  test "$3" = generate &&
  test "$4" = --template &&
  test "$5" = buf.gen.local.yaml; then
  : >"$LOCAL_TEMPLATE_MARKER"
fi
exec "$REAL_GO" "$@"
EOF
chmod +x "$tmp/bin/go"

printf '%s\n' 'real fallback test: network/module-cache access is allowed and bounded to 10 minutes' >&2
PATH="$tmp/bin:$PATH" REAL_GO="$real_go" INSTALL_LOG="$tmp/install.log" REMOTE_OUTAGE_MARKER="$tmp/remote-outage" LOCAL_TEMPLATE_MARKER="$tmp/local-template" VERIFY_GENERATION_REMOTE_ATTEMPTS=1 ruby "$tmp/timeout.rb" 600 2 sh "$ROOT/scripts/verify_generation.sh" >"$tmp/output.log" 2>&1 || {
  status=$?
  cat "$tmp/output.log" >&2
  exit "$status"
}

grep -q 'recognized remote availability/rate-limit failure; using local fallback' "$tmp/output.log"
grep -q 'installing exact local protoc plugins' "$tmp/output.log"
grep -q 'local fallback generation' "$tmp/output.log"
test -e "$tmp/remote-outage"
test -e "$tmp/local-template"

manifest="$tmp/manifest"
template="$tmp/template.yaml"
ruby "$ROOT/scripts/derive_buf_gen_local.rb" "$ROOT/buf.gen.yaml" "$template" "$manifest"
ruby -ryaml -e '
  plugins=YAML.safe_load_file(ARGV[0], aliases: false).fetch("plugins")
  installs=File.readlines(ARGV[1], chomp: true).map { |line| line.split("\t", 2) }
  actual_installs=File.readlines(ARGV[2], chomp: true)
  expected={
    "protoc-gen-go" => "google.golang.org/protobuf/cmd/protoc-gen-go",
    "protoc-gen-go-grpc" => "google.golang.org/grpc/cmd/protoc-gen-go-grpc",
    "protoc-gen-grpc-gateway" => "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway",
    "protoc-gen-openapiv2" => "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2"
  }
  abort unless plugins.length == installs.length
  abort unless actual_installs == installs.map { |mod, version| "#{mod}@#{version}" }
  plugins.zip(installs).each do |plugin, (mod, version)|
    abort unless expected.fetch(plugin.fetch("local")) == mod
    abort unless version.match?(/\Av(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:[-+][0-9A-Za-z.-]+)?\z/)
  end
' "$template" "$manifest" "$tmp/install.log"

printf '%s\n' 'ok - real exact-version local fallback generated and compared all artifacts'
