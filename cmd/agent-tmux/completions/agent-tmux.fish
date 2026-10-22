# fish completion for agent-tmux.
#
# Install:  agent-tmux completion fish | source      (or `make install-completions`)
#
# DO NOT add command, flag or session names to this file. It only relays the
# words typed so far to `agent-tmux __complete`, which derives every candidate
# from the CLI's own command registry. A test fails if this file starts
# duplicating that knowledge.

function __agent_tmux_complete
    set -l tokens (commandline -opc)
    set -e tokens[1]
    # Quoted so an empty current word is still passed as one (empty) argument.
    set -l current (commandline -ct)
    agent-tmux __complete $tokens "$current" 2>/dev/null
end

# Output is "candidate<TAB>description", which is exactly what fish expects.
complete -c agent-tmux -f -a '(__agent_tmux_complete)'
