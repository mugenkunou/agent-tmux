# bash completion for agent-tmux.
#
# Install:  eval "$(agent-tmux completion bash)"      (or `make install-completions`)
#
# DO NOT add command, flag or session names to this file. It only relays the
# words typed so far to `agent-tmux __complete`, which derives every candidate
# from the CLI's own command registry. A test fails if this file starts
# duplicating that knowledge.

_agent_tmux() {
    local cur=${COMP_WORDS[COMP_CWORD]} i line
    local -a args=()

    # bash splits "--flag=value" into three words; fold the "=" away so the
    # binary sees the same words zsh and fish give it.
    for ((i = 1; i < COMP_CWORD; i++)); do
        [[ ${COMP_WORDS[i]} == "=" ]] || args+=("${COMP_WORDS[i]}")
    done
    [[ $cur == "=" ]] && cur=""
    args+=("$cur")

    COMPREPLY=()
    # Output is "candidate" or "candidate<TAB>description"; bash has no
    # place to show descriptions, so keep only the candidate.
    while IFS= read -r line; do
        COMPREPLY+=("${line%%$'\t'*}")
    done < <("${COMP_WORDS[0]}" __complete "${args[@]}" 2>/dev/null)
}

complete -F _agent_tmux agent-tmux
