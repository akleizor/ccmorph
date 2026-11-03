# The zsh suite's library: the shipped one, then the test vocabulary on top.
# Either source failing (an empty env-key list fails the second, #141) fails
# this one, so a caller that ignores the status still loses the vocabulary.
source "${${(%):-%x}:A:h:h}/lib/ccmorph.sh" || return
source "${${(%):-%x}:A:h}/core_helpers.zsh" || return
