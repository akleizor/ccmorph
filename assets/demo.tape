# Re-record from the repo root: vhs assets/demo.tape
Output assets/demo.gif
Set Shell bash
Set FontSize 17
Set Width 960
Set Height 500
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
Sleep 3s
Type "# Start a session on work" Enter
Type "ccmorph run work" Enter
Sleep 1.5s
Type "add a --verbose flag to the CLI" Enter
Sleep 3s
Type "# Out of quota? Move this conversation to the other profile" Enter
Sleep 2s
Type "! ccmorph hop personal" Enter
Sleep 3s
Type "/exit" Enter
Sleep 3.5s
Type "# Same conversation, now on personal" Enter
Sleep 2s
Type "now add a test for it" Enter
Sleep 4s
