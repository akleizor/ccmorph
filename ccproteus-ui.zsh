# Standalone zsh UI: no profiles, persistence, authentication or app dependencies.
# ccs_ui_select native|search TITLE ROWS [labels|all] -> selected ID on stdout.
# ROWS: ID<TAB>label[<TAB>description]; `all`: ID doubles as the visible label.
# ccs_ui_input PROMPT [secret] -> REPLY (never print a credential).
# ccs_ui_confirm PROMPT -> 0 yes, 1 no, 2 cancelled; defaults to No.

# Claude Code 2.1.278 dark-theme roles: brand, suggestion, inactive and status.
# Keep native ANSI and fzf colors derived from this one palette. Body text and
# background inherit the terminal; NO_COLOR and TERM=dumb remain unstyled.
_ccs_ui_theme() {
  # The palette walks below ("${(@kv)...}", "${(@k)...}") and the hex subscripts
  # assume zsh's native arrays; this runs at source time with the caller's own
  # options, and a caller's KSH_ARRAYS would build the roles from values, not
  # keys. localoptions keeps the change inside this function.
  setopt localoptions noksharrays
  local -A palette=(accent d77757 focus b1b9f9 gray 999999 border 888888
                    yellow ffc107 red ff6b80)
  local role hex
  if [[ -t "${1:-1}" && -z "${NO_COLOR:-}" && "${TERM:-}" != dumb ]]; then
    _ccs_bold=$'\033[1m'  _ccs_dim=$'\033[2m'  _ccs_reset=$'\033[0m'
    for role hex in "${(@kv)palette}"; do
      printf -v "_ccs_$role" '\033[38;2;%d;%d;%dm' \
        "$(( 16#${hex[1,2]} ))" "$(( 16#${hex[3,4]} ))" "$(( 16#${hex[5,6]} ))"
    done
    # Existing non-interactive summaries use this older name for emphasis.
    _ccs_cyan="$_ccs_focus"
  else
    _ccs_bold='' _ccs_dim='' _ccs_reset='' _ccs_cyan=''
    for role in "${(@k)palette}"; do printf -v "_ccs_$role" '%s' ''; done
  fi
  # Already-running pre-update launch drivers still read this name after reload.
  _ccs_green="$_ccs_accent"
  _ccs_fzf_theme="fg:-1,bg:-1,hl:#${palette[focus]},fg+:#${palette[focus]},bg+:-1,hl+:#${palette[focus]},pointer:#${palette[focus]},marker:#${palette[focus]},prompt:#${palette[accent]},header:#${palette[gray]},info:#${palette[gray]},border:#${palette[border]}"
}
_ccs_ui_theme
# Entry points theme their own stream; these names stay local to each call.
typeset -ga _ccs_ui_vars=(_ccs_bold _ccs_dim _ccs_reset _ccs_green _ccs_yellow _ccs_red
  _ccs_cyan _ccs_gray _ccs_accent _ccs_focus _ccs_border _ccs_fzf_theme)

# One left edge: headings, labels, hints and a numbered list's `1)` index start
# after the margin; only the ❯ pointer hangs in it. Descriptions follow the widest
# label after a fixed gap. Every width below derives from these two values.
typeset -g _ccs_ui_margin='  '
typeset -gi _ccs_ui_gap=4

_ccs_ui_error() { print -ru2 -- "${_ccs_red}ccproteus:${_ccs_reset} $*"; }

# Application call sites choose an interaction, never a renderer or theme.
ccs_ui_select() {
  emulate -L zsh
  local mode="$1" title="$2" rows="$3" display="${4:-labels}"
  local "${_ccs_ui_vars[@]}"
  _ccs_ui_theme 2
  # IDs are returned verbatim, but must never contain terminal controls.
  local line value
  for line in "${(@f)rows}"; do
    value="${line%%$'\t'*}"
    [[ -n "$value" && "$value" != *[[:cntrl:]]* ]] || {
      _ccs_ui_error 'invalid choice ID'; return 2
    }
  done
  case "$mode" in
    native) _ccs_ui_native "$title" "$rows" "$display" ;;
    search)
      if [[ "$display" == all ]]; then
        local row labeled=""
        for row in "${(@f)rows}"; do
          [[ -n "$row" ]] || continue
          [[ -z "$labeled" ]] || labeled+=$'\n'
          labeled+="${row%%$'\t'*}"$'\t'"$row"
        done
        rows="$labeled"
      fi
      _ccs_ui_search "$title" "$rows"
      ;;
    *) _ccs_ui_error "unknown UI mode: $mode"; return 2 ;;
  esac
}

# One label renderer for native, searchable, and numbered menus. Machine values never
# acquire ANSI escapes; descriptions stay visually secondary in either UI.

# The visible part of a row: ID<TAB>label[<TAB>description], minus the ID for `labels`.
_ccs_ui_visible() {
  REPLY="$1"
  [[ "${2:-all}" != labels ]] || REPLY="${1#*$'\t'}"
}

# Terminal-cell widths, not bytes or character counts (CJK and combining marks).
# A byte locale (cron, launchd, bare ssh) would count and cut UTF-8 bytes, so
# measure in UTF-8 regardless; the caller's locale is untouched.
# ponytail: assumes C.UTF-8 exists (macOS, glibc >= 2.35); older hosts still count bytes.
_ccs_ui_cells() {
  [[ ${(m)#:-—} == 1 ]] || local LC_ALL=C.UTF-8
  REPLY=${(m)#1}
}

_ccs_ui_width() {
  local rows="$1" display="${2:-all}" row width=0
  for row in "${(@f)rows}"; do
    _ccs_ui_visible "$row" "$display"
    _ccs_ui_cells "${${REPLY%%$'\t'*}//[[:cntrl:]]/ }"
    (( REPLY <= width )) || width=$REPLY
  done
  REPLY=$width
}

_ccs_ui_fit() {
  [[ ${(m)#:-—} == 1 ]] || local LC_ALL=C.UTF-8
  REPLY="${1//[[:cntrl:]]/ }"
  local width="$2"
  (( width < 1 )) && { REPLY=''; return; }
  if (( ${(m)#REPLY} > width )); then
    while (( ${(m)#REPLY} >= width )); do REPLY="${REPLY[1,-2]}"; done
    REPLY+='…'
  fi
}

# reply=(rows columns) of the terminal stderr paints on. `stty size` reports 0 0
# on some ptys; unknown sizes fall back to 24x80. Output that is not a terminal
# is never cut to a guessed width.
_ccs_ui_size() {
  local -a size
  if [[ -t 2 ]]; then
    size=(${=:-$(command stty size </dev/tty 2>/dev/null)})
    [[ "${size[1]:-}" == <1-> ]] || size[1]=24
    [[ "${size[2]:-}" == <1-> ]] || size[2]=80
  else
    size=(24 32767)
  fi
  reply=("${size[@]}")
}

# Headings are fitted to the screen here, so no caller can overflow the edge.
_ccs_ui_heading() {
  local columns="${2:-}"
  [[ -n "$columns" ]] || { _ccs_ui_size; columns=${reply[2]}; }
  _ccs_ui_fit "$1" "$(( columns - ${#_ccs_ui_margin} ))"
  REPLY="${_ccs_ui_margin}${_ccs_accent}${_ccs_bold}${REPLY}${_ccs_reset}"
}

# ROW is label[<TAB>description]; WIDTH is the label column; ROW_WIDTH, when
# given, is every cell after the pointer/index, and the description is fitted
# into what the label column and gap leave of it.
_ccs_ui_label() {
  local row="$1" width="$2" focused="${3:-0}" row_width="${4:-}" label detail="" padding
  _ccs_ui_fit "${row%%$'\t'*}" "$width"; label="$REPLY"
  if [[ "$row" == *$'\t'* ]]; then
    detail="${row#*$'\t'}"
    [[ -z "$row_width" ]] || { _ccs_ui_fit "$detail" "$(( row_width - width - _ccs_ui_gap ))"; detail="$REPLY"; }
    _ccs_ui_cells "$label"
    [[ -z "$detail" ]] || printf -v padding '%*s' "$(( width - REPLY + _ccs_ui_gap ))" ''
  fi
  # Selection is shown by colour and the pointer, not weight (clack, gum, gh).
  REPLY="$label"
  (( focused )) && REPLY="${_ccs_focus}${label}${_ccs_reset}"
  [[ -z "$detail" ]] || REPLY+="${padding}${_ccs_gray}${detail//[[:cntrl:]]/ }${_ccs_reset}"
}

# Non-interactive instructions share the label renderer; callers keep their
# existing scripted input vocabulary and validate it at the application boundary.
ccs_ui_list() {
  emulate -L zsh
  local prompt="$1" rows="$2" display="${3:-labels}" row
  local "${_ccs_ui_vars[@]}"
  _ccs_ui_theme 2
  local -a options=("${(@f)rows}")
  local i=1 width columns index="${#${#options}}"
  _ccs_ui_size; columns=${reply[2]}
  local row_width=$(( columns - ${#_ccs_ui_margin} - index - 2 ))
  _ccs_ui_width "$rows" "$display"
  width=$(( REPLY < row_width ? REPLY : row_width ))
  _ccs_ui_heading "$prompt" "$columns"
  print -ru2 -- "$REPLY"
  print -u2
  for row in "${options[@]}"; do
    _ccs_ui_visible "$row" "$display"
    _ccs_ui_label "$REPLY" "$width" 0 "$row_width"
    printf '%s%*s) %s\n' "$_ccs_ui_margin" "$index" "$i" "$REPLY" >&2
    (( ++i ))
  done
}

# Rows are value<TAB>description, or value<TAB>label<TAB>description for
# labeled actions. Only values go to stdout; menus use stderr.
_ccs_ui_numbered() {
  emulate -L zsh
  local prompt="$1" rows="$2" display="${3:-all}" choice="" row
  local -a options=("${(@f)rows}")
  [[ -n "$rows" ]] || { _ccs_ui_error "no available choices"; return 1; }
  ccs_ui_list "$prompt" "$rows" "$display"
  while true; do
    ccs_ui_input "Select [1-${#options}] (q to cancel): " || return 1
    choice="$REPLY"
    [[ "$choice" == (q|Q) ]] && return 1
    if [[ -z "$choice" ]]; then
      print -ru2 -- 'Please choose one of the numbered options.'
      continue
    fi
    if [[ "$choice" == <-> ]] && (( choice >= 1 && choice <= ${#options} )); then
      print -r -- "${options[$choice]%%$'\t'*}"
      return 0
    fi
    print -ru2 -- 'Please choose one of the numbered options.'
  done
}

# Fixed choices are an inline native menu, not a search interface. Zsh owns
# key reads; the existing label renderer owns typography. No external selector.
_ccs_ui_native() {
  emulate -L zsh
  setopt localtraps
  local prompt="$1" rows="$2" display="${3:-labels}" key="" next="" row label
  local -a options=("${(@f)rows}")
  [[ -n "$rows" ]] || { _ccs_ui_error 'no available choices'; return 1; }
  [[ -t 0 && -t 2 ]] || {
    _ccs_ui_error 'selection needs a terminal; pass the value directly'
    print -ru2 -- "$rows"
    return 1
  }
  if [[ "${TERM:-}" == dumb ]]; then
    _ccs_ui_numbered "$prompt" "$rows" "$display"
    return $?
  fi
  local selected=1 height=20 first last i drawn=0 result=1 tty_state width columns resized=1 heading
  _ccs_ui_width "$rows" "$display"; width=$REPLY
  tty_state=$(command stty -g </dev/tty) || return 1
  # `read -rsk` does not abort on a caught signal — zsh runs the handler and
  # resumes the read. Use a flag the loop checks after each read attempt, so
  # the always block still runs and the exit status survives the unwind.
  local interrupted=0
  trap 'interrupted=130' INT
  trap 'interrupted=143' TERM
  trap 'interrupted=129' HUP
  trap 'resized=1' WINCH
  # Ctrl-C is read as a key, not a signal to the parent command substitution.
  command stty -echo -icanon -isig min 1 time 0 </dev/tty || return 1
  # Disable wrapping while painting so long labels cannot corrupt cursor math.
  # Restore terminal modes on selection, cancellation, EOF, and handled signals.
  print -nu2 -- $'\e[?25l\e[?7l'
  {
    local row_width label_width
    while true; do
      # Size and heading change only on resize, not on every keystroke.
      if (( resized )); then
        _ccs_ui_size
        columns=${reply[2]}
        # Heading, blank, blank and hint surround the rows; keep one row minimum.
        height=$(( reply[1] > 5 ? reply[1] - 5 : 1 ))
        row_width=$(( columns - ${#_ccs_ui_margin} ))
        # Leave room for the gap and some description after the label column.
        label_width=$(( width < row_width - _ccs_ui_gap - 1 ? width : row_width - _ccs_ui_gap - 1 ))
        (( label_width > 0 )) || label_width=1
        _ccs_ui_heading "$prompt" "$columns"; heading="$REPLY"
        resized=0
      fi
      (( drawn == 0 )) || printf '\033[%dA\r' "$drawn" >&2
      first=$(( selected > height ? selected - height + 1 : 1 ))
      last=$(( first + height - 1 < ${#options} ? first + height - 1 : ${#options} ))
      print -ru2 -- $'\e[2K'"$heading"
      print -ru2 -- $'\e[2K'
      for (( i=first; i<=last; i++ )); do
        _ccs_ui_visible "${options[$i]}" "$display"
        _ccs_ui_label "$REPLY" "$label_width" "$(( i == selected ))" "$row_width"
        if (( i == selected )); then label="${_ccs_focus}❯${_ccs_reset}"; else label=" "; fi
        print -ru2 -- $'\e[2K'"$label $REPLY"
      done
      print -ru2 -- $'\e[2K'
      # Autowrap is off while painting, so the terminal clips this fixed hint.
      print -ru2 -- $'\e[2K'"${_ccs_ui_margin}${_ccs_gray}↑/↓ Move · Enter Select · Esc Cancel${_ccs_reset}"
      drawn=$(( last - first + 5 ))
      read -rsk 1 key || { (( resized )) && continue; break; }
      (( interrupted == 0 )) || break
      case "$key" in
        $'\r'|$'\n') result=0; break ;;
        $'\003') interrupted=130; break ;;
        $'\004') break ;;
        $'\e')
          read -rsk 1 -t 0.08 next || break
          [[ "$next" == ('['|O) ]] || break
          read -rsk 1 -t 0.08 next || break
          case "$next" in
            A) (( selected <= 1 )) || (( --selected )) ;;
            B) (( selected >= ${#options} )) || (( ++selected )) ;;
          esac
          ;;
      esac
    done
  } always {
    command stty "$tty_state" </dev/tty 2>/dev/null || :
    (( drawn == 0 )) || printf '\033[%dA\r\033[J' "$drawn" >&2
    print -nu2 -- "${_ccs_reset}"$'\e[?7h\e[?25h'
  }
  (( interrupted == 0 )) || return "$interrupted"
  (( result == 0 )) || { print -ru2 -- "${_ccs_ui_margin}${_ccs_gray}Cancelled${_ccs_reset}"; return 1; }
  _ccs_ui_visible "${options[$selected]}" "$display"
  _ccs_ui_receipt "$prompt" "${REPLY%%$'\t'*}"
  print -r -- "${options[$selected]%%$'\t'*}"
}

# A one-line receipt (Title › choice), so chained menus stack as a tidy log. The
# choice keeps priority; the title gives way first on a narrow screen.
_ccs_ui_receipt() {
  local title="$1" choice columns
  _ccs_ui_size; columns=${reply[2]}
  _ccs_ui_fit "$2" "$(( columns / 2 ))"; choice="$REPLY"
  _ccs_ui_cells "$choice"
  _ccs_ui_heading "$title" "$(( columns - REPLY - 3 ))"
  print -ru2 -- "$REPLY ${_ccs_focus}›${_ccs_reset} ${_ccs_gray}${choice}${_ccs_reset}"
}

_ccs_ui_search() {
  local prompt="$1" rows="$2" choice="" row rendered="" width
  [[ -n "$rows" ]] || { _ccs_ui_error 'no available choices'; return 1; }
  if [[ "${TERM:-}" != dumb ]] && whence -p fzf >/dev/null 2>&1; then
    _ccs_ui_width "$rows" labels; width=$REPLY
    for row in "${(@f)rows}"; do
      _ccs_ui_label "${row#*$'\t'}" "$width"
      [[ -z "$rendered" ]] || rendered+=$'\n'
      rendered+="${row%%$'\t'*}"$'\t'"$REPLY"
    done
    local -a theme=(--color "$_ccs_fzf_theme")
    [[ -z "${NO_COLOR:-}" ]] || theme=(--no-color)
    # User fzf defaults must not change selection cardinality, execute bindings,
    # or leak query/expect lines into the selected ID.
    choice=$(print -r -- "$rendered" | FZF_DEFAULT_OPTS= FZF_DEFAULT_OPTS_FILE= command fzf \
      --height 45% --reverse --border=rounded --pointer='❯' --marker='✓' \
      --no-multi --no-print-query --ansi "${theme[@]}" \
      --delimiter '\t' --with-nth=2.. --prompt "$prompt> " \
      --header '↑/↓ Move · Type to filter · Enter Select · Esc Cancel') || return 1
    [[ -n "$choice" ]] || return 1
    local value="${choice%%$'\t'*}" found=0
    for row in "${(@f)rows}"; do
      [[ "${row%%$'\t'*}" == "$value" ]] && found=1
    done
    (( found )) || { _ccs_ui_error 'invalid selection'; return 1; }
    _ccs_ui_receipt "$prompt" "${${choice#*$'\t'}%%$'\t'*}"
    print -r -- "$value"
  elif [[ -t 0 ]]; then
    _ccs_ui_numbered "$prompt" "$rows" labels
  else
    _ccs_ui_error 'selection needs a terminal or fzf; pass the value directly'
    print -ru2 -- "$rows"
    return 1
  fi
}

# Canonical read waits for Enter even after Esc. Read keys for terminal input
# instead; keep normal line reads for pipes, where escape bytes are data.
_ccs_ui_readline() {
  local mode="$1" value='' key next removed count
  REPLY=''
  while read -rsk 1 key; do
    case "$key" in
      $'\r'|$'\n') REPLY="$value"; return 0 ;;
      $'\003') return 130 ;;
      $'\004') return 1 ;;
      $'\e')
        read -rsk 1 -t 0.08 next || return 1
        [[ "$next" == ('['|O) ]] || return 1
        # Terminal key sequences are not bare Escape.
        while read -rsk 1 -t 0.08 next; do
          [[ "$next" == [[:alpha:]~] ]] && break
        done
        ;;
      $'\177'|$'\b'|$'\025'|$'\027')
        removed="${value[-1]}"
        case "$key" in
          $'\025') removed="$value"; value='' ;;
          $'\027')
            local rest="${value%%[[:space:]]#}"
            rest="${rest%%[^[:space:]]#}"
            removed="${value[${#rest}+1,-1]}"; value="$rest"
            ;;
          *) value="${value[1,-2]}" ;;
        esac
        if [[ "$mode" != secret ]]; then
          for (( count=0; count<${(m)#removed}; count++ )); do print -nu2 -- $'\b \b'; done
        fi
        ;;
      *)
        [[ "$key" == [[:cntrl:]] ]] && continue
        value+="$key"
        [[ "$mode" == secret ]] || print -nu2 -- "$key"
        ;;
    esac
  done
  return 1
}

# Text entry owns echo behavior and the same heading/chevron as selections.
# REPLY is deliberate: command substitution must never print a secret.
ccs_ui_input() {
  # _ccs_ui_size and _ccs_ui_readline index 1-based arrays like the rest of the
  # library, and the heading path from here is the only one without an
  # `emulate -L zsh` above it. Under a caller's KSH_ARRAYS the size read shifts,
  # the 80-column fallback lands where the column count is read, and the heading
  # is cut to 80 columns on a wider terminal. localoptions keeps this local.
  setopt localoptions localtraps extendedglob noksharrays
  local prompt="$1" mode="${2:-text}" result=0 tty_state='' interrupted=0
  local "${_ccs_ui_vars[@]}"
  _ccs_ui_theme 2
  # Read Esc immediately, keep secrets hidden, and treat Ctrl-C as a key so it
  # cannot signal the parent profile-lock process. Restore modes on every exit.
  if [[ -t 0 ]]; then
    tty_state=$(command stty -g) || return 1
    command stty -echo -icanon -isig min 1 time 0 || return 1
    # `return` from a trap inside an `always` block loses the exit status —
    # the handler runs, the always block runs, and the function continues with
    # result=0 and a partial value in REPLY. Use a flag the caller checks
    # after the block, so the signal's status survives the unwind.
    trap 'interrupted=130' INT
    trap 'interrupted=143' TERM
    trap 'interrupted=129' HUP
  fi
  {
    if [[ -t 0 && -t 2 && "${TERM:-}" != dumb ]]; then
      _ccs_ui_heading "$prompt"
      print -nu2 -- "$REPLY"$'\n'"${_ccs_ui_margin}${_ccs_gray}Enter Submit · Esc Cancel${_ccs_reset}"$'\n'"${_ccs_focus}❯${_ccs_reset} "
    elif [[ -t 0 ]]; then
      print -nu2 -- "$prompt (Enter Submit · Esc Cancel) "
    else
      print -nu2 -- "$prompt"
    fi
    REPLY=''
    if [[ -t 0 ]]; then
      _ccs_ui_readline "$mode" || result=$?
      print -u2
      if (( result )); then
        REPLY=''
        print -ru2 -- "${_ccs_ui_margin}${_ccs_gray}Cancelled${_ccs_reset}"
      fi
    else
      read -r REPLY || result=$?
      # `read` returns non-zero at EOF even when it read data. Treat "read
      # something, then EOF" as success rather than discarding the value.
      (( result )) && [[ -n "$REPLY" ]] && result=0
      [[ "$mode" != secret ]] || print -u2
    fi
  } always {
    [[ -z "$tty_state" ]] || command stty "$tty_state"
  }
  (( interrupted == 0 )) || { REPLY=''; return "$interrupted"; }
  return "$result"
}

ccs_ui_confirm() {
  emulate -L zsh
  local answer
  if [[ -t 0 && -t 2 && "${TERM:-}" != dumb ]]; then
    answer=$(ccs_ui_select native "$1" $'n\tNo\ny\tYes') || return 2
  else
    ccs_ui_input "$1 [y/N] " || return 2
    answer="$REPLY"
  fi
  [[ "${answer:l}" == (y|yes) ]]
}
