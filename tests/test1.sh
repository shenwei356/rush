#!/bin/bash

test -e ssshtest || wget -q https://raw.githubusercontent.com/ryanlayer/ssshtest/master/ssshtest

. ssshtest
set -e


go build -o rush
app=./rush

set +e

# -------------------------------------------------

# basic
fn_check_basic() {
    seq 1 10 | $app echo {}
}
run basic fn_check_basic
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | wc -l) 10

# keep order
fn_check_keep_order() {
    seq 1 10 | $app echo {} -k
}
run keep_order fn_check_keep_order
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | sort -k1,1n | paste -s -d " " - | sed 's/ //g') '12345678910'

# lots of output
fn_check_many_output() {
    seq 1 10 | $app 'seq 1000000'
}
run check_many_output fn_check_many_output
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | wc -l) 10000000

# lots of output + keep order
fn_check_many_output_keep_order() {
    seq 1 10 | $app 'seq 1000000' -k
}
run fn_check_many_output_keep_order fn_check_many_output_keep_order
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | wc -l) 10000000

# keep order + -n
fn_check_keep_order() {
    seq 1 10 | $app 'echo {1}' -k -j 2 -n 2
}
run keep_order_n fn_check_keep_order
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | sort -k1,1n | paste -s -d " " - | sed 's/ //g') '13579'

# -------------------------------------------------

# verbose
fn_check_verbose() {
    seq 1 10 | $app echo {} --verbose
}
run verbose fn_check_verbose
assert_equal $(cat $STDOUT_FILE | wc -l) 10
assert_equal $(cat $STDERR_FILE | grep "start" | wc -l) 10
assert_equal $(cat $STDERR_FILE | grep "finish" | wc -l) 10

# -------------------------------------------------

# replacement strings
fn_check_replacement_strings() {
    echo 123 dir/file.txt.gz | $app 'echo job {#} {1} {2} {2.} {2:} {2/} {2%.}'
}
run check_replacement_strings fn_check_replacement_strings
assert_no_stderr
assert_equal $(cat $STDOUT_FILE | sed 's/ //g' | tr -d '\n') "job1123dir/file.txt.gzdir/file.txtdir/filedirfile.txt"

# retry
fn_check_retry() {
    seq 5 | $app 'python jhz.py' -r 2
}
run retry fn_check_retry
assert_no_stdout
assert_in_stderr "ERRO"
assert_equal $(cat $STDERR_FILE | grep "WARN" | wc -l) 10
assert_equal $(cat $STDERR_FILE | grep "ERRO" | wc -l) 5

# exit on first err 
fn_check_exit_on_first_err() {
    seq 5 | $app 'python jhz.py' -e
}
# run check_exit_on_first_err fn_check_exit_on_first_err
# assert_no_stdout
# assert_in_stderr "first"

# -------------------------------------------------

# time out
fn_check_timeout() {
    seq 1 10 | $app -t 1 "sleep 2; echo asdf"
}
run time_out fn_check_timeout
assert_no_stdout
assert_in_stderr "time out"
assert_equal $(cat $STDERR_FILE | grep "time out" | wc -l) 10

# time out 2, timeout > running time
fn_check_timeout2() {
    seq 1 10 | $app -t 2 "sleep 1; echo asdf"
}
run time_out2 fn_check_timeout2
assert_no_stderr
assert_in_stdout "asdf"
assert_equal $(cat $STDOUT_FILE | grep "asdf" | wc -l) 10

# -------------------------------------------------

# continue
fn_check_continue() {
    seq 1 10 | $app 'echo {}' -c -C t.rush
    seq 1 10 | $app 'echo {}' -c -C t.rush
    rm t.rush
}
run continue fn_check_continue
assert_equal $(cat $STDERR_FILE | grep "ignore" | wc -l) 10

# continue mutli-line cmds
fn_check_continue() {
    seq 1 10 | $app 'echo {};\
        echo s{}' -c -C t2.rush
    seq 1 10 | $app 'echo {};\
        echo s{}' -c -C t2.rush
    rm t2.rush
}
run continue fn_check_continue
assert_equal $(cat $STDERR_FILE | grep "ignore" | wc -l) 10
