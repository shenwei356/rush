#!/usr/bin/env python

import sys
import time
import os


def timeit(cmd):
    t = time.time()
    os.system(cmd)
    return time.time() - t

# test name, input command, test command
tests = [
    ['a few stdout', 'seq 1 10', 'echo job:{}; seq 1 10'],
    ['lots of stdout', 'seq 1 10', 'echo job:{}; seq 1 1000000'],
    ['a few stdout & run long', 'seq 1 10', 'echo job:{}; sleep 2; seq 1 10'],
    ['lots of stdout & run long', 'seq 1 10', 'echo job:{}; sleep 2; seq 1 1000000'],
]

apps = {'parallel': {'jobs': '-j', 'keep-order': '-k'},
        'gargs': {'jobs': '-p', 'keep-order': '-o'},
        './rush': {'jobs': '-j', 'keep-order': '-k'}}

repeats = 1
njobs = 4

for test in tests:
    n = int((75 - len(test[0])) / 2)
    sys.stderr.write('{}[ {} ]{}\n'.format('=' * n,  test[0], '=' * n))

    for keep_order in [False, True]:
        msg = 'keep order: {}'.format(keep_order)
        n = int((70 - len(msg)) / 2)
        sys.stderr.write('{}[ {} ]{}\n'.format('-' * n,  msg, '-' * n))

        sys.stderr.write('\ncommands:\n')
        times = dict()
        for app, args in apps.items():
            cmd = '{} | {} {} {} {} "{}" > t.{}'.format(
                test[1], app, args['jobs'], njobs,
                args['keep-order'] if keep_order else '',
                test[2], os.path.basename(app))

            sys.stderr.write('{}\n'.format(cmd))

            times[app] = timeit(cmd)

        sys.stderr.write('\ntime:\n')
        for app in sorted(times.keys()):
            sys.stderr.write('{}: {}\n'.format(app, times[app]))

        sys.stderr.write('\nmd5sum:\n')
        for app in sorted(times.keys()):
            os.system('md5sum t.{}'.format(os.path.basename(app)))
            os.remove('t.{}'.format(os.path.basename(app)))

        sys.stderr.write('\n')
    sys.stderr.write('\n')
