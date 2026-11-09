# Re-record from the repo root (after `make shim` builds libexec/ccmorph-core):
#   uv run --with pyte --with pillow python assets/demo/record.py
Output assets/demo.gif
Set Shell bash
Set FontSize 17
Set Width 960
Set Height 560
Set Padding 24
Set Margin 20
Set MarginFill "#312e81"
Set BorderRadius 10
Set WindowBar Colorful
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Framerate 12

Hide
Type "source assets/demo/setup.sh" Enter
Show

Type "# Three profiles: a subscription, an API key, a gateway" Enter
Type "ccmorph list" Enter
Sleep 1s
Type "# Start a session on work" Enter
Type "ccmorph run work" Enter
Sleep 1s
Type "add a --verbose flag to the CLI" Enter
Sleep 1s
Type "! ccmorph profile" Enter
Sleep 1s
Type "! ccmorph hop personal" Enter
Sleep 2s
Type "now add a test for it" Enter
Sleep 1s
Type "/exit" Enter
Sleep 1s
