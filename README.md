# rush -- parallelly execute shell commands

## Features

Major:

- [ ] argument delmiter (`-d`, default `\n`) and
  lines sending to every command (`-n`, default `1`)
- [ ] keep output order, may use temporary file
- [ ] support timeout and retry
- [ ] support continue,
  save status after [capturing ctrl+c](https://nathanleclaire.com/blog/2014/08/24/handling-ctrl-c-interrupt-signal-in-golang-programs/)
- [ ] support positional replacement strings: `{n}`
    - [ ] columns in delimiter-delimited data
    - [ ] matches of regular expression
- [ ] GNU parallel like replacement strings:
    - [ ] `{@}`, job slot number (`{%}` in GNU parallel)
    - [ ] `{#}`, job number
    - [ ] `{}`, full line
    - [ ] `{.}`, input line without the last extension
    - [ ] `{,}`, input line without any extension (GNU parallel does not have)
    - [ ] `{/}`, dirname  (`{//}` in GNU parallel)
    - [ ] `{%}`, basename (`{/}` in GNU parallel)
    - [ ] possible combinations:
        - [ ] `{%.}`, `{%,}`
        - [ ] `{n.}`, `{n/}` ...
- [ ] `awk -v` like defined variables

Minor:

- [ ] logging
- [ ] dry run
- [ ] exit on error


## Contact

[Create an issue](https://github.com/shenwei356/rush/issues) to report bugs,
propose new functions or ask for help.

## License

[MIT License](https://github.com/shenwei356/rush/blob/master/LICENSE)
