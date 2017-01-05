#!/usr/bin/env python

import sys
import time
import os


def timeit(cmd):
    sys.stderr.write('{}\n'.format(cmd))
    t = time.time()
    os.system(cmd)
    return time.time() - t

tests = [['seq 1 10', 'echo job:{}; seq 1 1000000']]

apps = {'parallel': {'jobs': '-j', 'keep-order': '-k'},
        'gargs': {'jobs': '-p', 'keep-order': '-o'},
        './rush': {'jobs': '-j', 'keep-order': '-k'}}

repeats = 1
njobs = 4

for test in tests:
    sys.stderr.write('-' * 79 + '\n')

    times = dict()
    for app, args in apps.items():
        cmd = '{} | {} {} {} {} "{}" > t.{}'.format(
            test[0], app, args['jobs'], njobs, args['keep-order'],
            test[1], os.path.basename(app))

        times[app] = timeit(cmd)

    sys.stderr.write('\ntime:\n')
    for app in sorted(times.keys()):
        sys.stderr.write('{}: {}\n'.format(app, times[app]))

    sys.stderr.write('\nmd5sum:\n')
    os.system('md5sum t.*')
