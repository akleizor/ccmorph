# The zsh suite's library: the shipped one, then the test vocabulary on top.
# Either source failing (an empty env-key list fails the second, #141) fails
# this one, so a caller that ignores the status still loses the vocabulary.
source "${${(%):-%x}:A:h:h}/lib/ccmorph.sh" || return
source "${${(%):-%x}:A:h}/core_helpers.zsh" || return

# #1026: a launch's own words must survive the test that ran it. A launch test
# silences a launch whose side effect is all it needs (`_ccm_launch ...
# >/dev/null 2>&1`), so when the launch itself is what failed, the sentence that
# says why went to /dev/null with it: the caps red of #1026 cost a scout
# investigation and still could not be named. run_test names a file for the
# test in CCMORPH_TEST_LAUNCH_LOG and prints it when the test fails; the
# wrappers below copy every launch's stdout and stderr there through tee. Each
# stream still reaches exactly the descriptor the test redirected it to -- a
# silenced launch stays silent, `out=$(...)` holds the same bytes -- and tee
# copies bytes, never lines, so no trailing-newline is rewritten. Only the
# launch entry points are wrapped; the core's other verbs keep their plain path.
#
# A stream that is a terminal is never wrapped: tee would hand the launcher a
# pipe where the test gave it a tty, and the launcher reads that (a terminal is
# what opens its picker and prints "Gateway ready", cli/run.go). The terminal
# tests already keep their child's whole pty output and print it on failure
# (_ccm_run_test_under_a_terminal), so nothing is lost there.
_ccm_test_capture_launch() {
  local log="${CCMORPH_TEST_LAUNCH_LOG:-}"
  if [[ -z "$log" ]]; then
    "$@"
    return $?
  fi
  local -i cap_out=1 cap_err=1
  [[ -t 1 ]] && cap_out=0
  [[ -t 2 ]] && cap_err=0
  if (( cap_out && cap_err )); then
    "$@" > >(command tee -a "$log") 2> >(command tee -a "$log" >&2)
  elif (( cap_out )); then
    "$@" > >(command tee -a "$log")
  elif (( cap_err )); then
    "$@" 2> >(command tee -a "$log" >&2)
  else
    "$@"
  fi
  return $?
}

# The plain functions stay reachable for a caller that must not be wrapped. The
# shipped library is re-sourced by every test, so each source re-copies whatever
# it just defined; the guard only steps aside if this file is sourced twice
# without the library between, when the current name is already the wrapper.
_ccm_test_wrap_launch_helper() {
  local name="$1"
  if [[ ${functions[$name]} == *"_ccm_test_capture_launch ${name}_plain"* ]]; then
    return 0
  fi
  functions[${name}_plain]="${functions[$name]}"
  functions[$name]="_ccm_test_capture_launch ${name}_plain \"\$@\""
}
_ccm_test_wrap_launch_helper _ccm_launch
_ccm_test_wrap_launch_helper _ccm_run_claude
_ccm_test_wrap_launch_helper _ccm_main_exec
