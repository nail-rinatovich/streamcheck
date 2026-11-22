"""Run a real fixture under a Windows Job Object hard process memory limit.
No third-party dependencies. Run from repository root after building binaries.
"""
import argparse
import ctypes as c
from ctypes import wintypes as w
import json
import msvcrt
import os
import subprocess
import sys
import time

class IO_COUNTERS(c.Structure):
    _fields_ = [(name, c.c_ulonglong) for name in (
        'ReadOperationCount', 'WriteOperationCount', 'OtherOperationCount',
        'ReadTransferCount', 'WriteTransferCount', 'OtherTransferCount')]

class BASIC_LIMIT(c.Structure):
    _fields_ = [('PerProcessUserTimeLimit', c.c_longlong), ('PerJobUserTimeLimit', c.c_longlong),
                ('LimitFlags', w.DWORD), ('MinimumWorkingSetSize', c.c_size_t),
                ('MaximumWorkingSetSize', c.c_size_t), ('ActiveProcessLimit', w.DWORD),
                ('Affinity', c.c_size_t), ('PriorityClass', w.DWORD), ('SchedulingClass', w.DWORD)]

class EXTENDED_LIMIT(c.Structure):
    _fields_ = [('BasicLimitInformation', BASIC_LIMIT), ('IoInfo', IO_COUNTERS),
                ('ProcessMemoryLimit', c.c_size_t), ('JobMemoryLimit', c.c_size_t),
                ('PeakProcessMemoryUsed', c.c_size_t), ('PeakJobMemoryUsed', c.c_size_t)]

class STARTUPINFO(c.Structure):
    _fields_ = [('cb', w.DWORD), ('lpReserved', w.LPWSTR), ('lpDesktop', w.LPWSTR),
                ('lpTitle', w.LPWSTR), ('dwX', w.DWORD), ('dwY', w.DWORD),
                ('dwXSize', w.DWORD), ('dwYSize', w.DWORD), ('dwXCountChars', w.DWORD),
                ('dwYCountChars', w.DWORD), ('dwFillAttribute', w.DWORD),
                ('dwFlags', w.DWORD), ('wShowWindow', w.WORD), ('cbReserved2', w.WORD),
                ('lpReserved2', c.POINTER(c.c_byte)), ('hStdInput', w.HANDLE),
                ('hStdOutput', w.HANDLE), ('hStdError', w.HANDLE)]

class PROCESSINFO(c.Structure):
    _fields_ = [('hProcess', w.HANDLE), ('hThread', w.HANDLE),
                ('dwProcessId', w.DWORD), ('dwThreadId', w.DWORD)]

def main():
    p = argparse.ArgumentParser()
    p.add_argument('--format', choices=['csv', 'jsonl'], default='csv')
    p.add_argument('--file-mib', type=int, default=256)
    p.add_argument('--limit-mib', type=int, default=64)
    a = p.parse_args()
    if a.file_mib <= a.limit_mib or a.limit_mib < 32:
        p.error('file-mib must exceed limit-mib; limit-mib must be >=32')
    os.makedirs('tmp', exist_ok=True)
    os.makedirs('reports', exist_ok=True)
    fixture = os.path.abspath('tmp/large.' + a.format)
    generated = subprocess.run(['bin/datagen.exe', '--format', a.format, '--bytes', str(a.file_mib << 20), '--output', fixture],
                               stderr=subprocess.PIPE, check=True, universal_newlines=True)
    print(generated.stderr.strip())
    k = c.WinDLL('kernel32', use_last_error=True)
    k.CreateJobObjectW.argtypes = [c.c_void_p, w.LPCWSTR]; k.CreateJobObjectW.restype = w.HANDLE
    k.SetInformationJobObject.argtypes = [w.HANDLE, c.c_int, c.c_void_p, w.DWORD]
    k.QueryInformationJobObject.argtypes = [w.HANDLE, c.c_int, c.c_void_p, w.DWORD, c.c_void_p]
    k.AssignProcessToJobObject.argtypes = [w.HANDLE, w.HANDLE]
    k.CreateProcessW.argtypes = [w.LPCWSTR, w.LPWSTR, c.c_void_p, c.c_void_p, w.BOOL, w.DWORD,
                               c.c_void_p, w.LPCWSTR, c.POINTER(STARTUPINFO), c.POINTER(PROCESSINFO)]
    k.ResumeThread.argtypes = [w.HANDLE]; k.ResumeThread.restype = w.DWORD
    k.WaitForSingleObject.argtypes = [w.HANDLE, w.DWORD]; k.WaitForSingleObject.restype = w.DWORD
    k.GetExitCodeProcess.argtypes = [w.HANDLE, c.POINTER(w.DWORD)]
    k.TerminateProcess.argtypes = [w.HANDLE, w.UINT]
    k.CloseHandle.argtypes = [w.HANDLE]
    def check(ok):
        if not ok: raise c.WinError(c.get_last_error())
    job = k.CreateJobObjectW(None, None); check(job)
    limits = EXTENDED_LIMIT()
    limits.BasicLimitInformation.LimitFlags = 0x100 | 0x2000  # PROCESS_MEMORY | KILL_ON_JOB_CLOSE
    limits.ProcessMemoryLimit = a.limit_mib << 20
    check(k.SetInformationJobObject(job, 9, c.byref(limits), c.sizeof(limits)))
    prefix = 'reports/memory-' + a.format
    os.environ['GOMEMLIMIT'] = '24MiB'  # GC target, separate from the enforced Job Object limit
    pi = PROCESSINFO()
    started = time.time()
    try:
        with open(prefix + '-report.json', 'wb') as out, open(prefix + '-stderr.txt', 'wb') as err, open(os.devnull, 'rb') as inp:
            si = STARTUPINFO(); si.cb = c.sizeof(si); si.dwFlags = 0x100
            handles = [msvcrt.get_osfhandle(f.fileno()) for f in (inp, out, err)]
            for handle in handles: os.set_handle_inheritable(handle, True)
            si.hStdInput, si.hStdOutput, si.hStdError = handles
            cmd = subprocess.list2cmdline([os.path.abspath('bin/streamcheck.exe'), 'validate', '--schema',
                                          'examples/schema.json', '--input', fixture, '--format', a.format, '--samples', '0'])
            check(k.CreateProcessW(None, c.create_unicode_buffer(cmd), None, None, True, 0x08000004, None, None, c.byref(si), c.byref(pi)))
            check(k.AssignProcessToJobObject(job, pi.hProcess))
            if k.ResumeThread(pi.hThread) == 0xffffffff: raise c.WinError(c.get_last_error())
            if k.WaitForSingleObject(pi.hProcess, 600000) != 0: raise RuntimeError('validator did not finish within 10 minutes')
            exit_code = w.DWORD(); check(k.GetExitCodeProcess(pi.hProcess, c.byref(exit_code)))
        measured = EXTENDED_LIMIT()
        check(k.QueryInformationJobObject(job, 9, c.byref(measured), c.sizeof(measured), None))
        with open(prefix + '-report.json', encoding='utf-8') as f: report = json.load(f)
        assert exit_code.value == 2, 'expected exit 2 because fixture has deliberate errors'
        assert report['complete'] and report['bytes_read'] == os.path.getsize(fixture)
        assert report['invalid_rows'] == report['rows'] // 1000
        assert report['errors'] == report['invalid_rows'] and report['parse_errors'] == 0
        assert measured.PeakProcessMemoryUsed <= a.limit_mib << 20
        result = dict(format=a.format, file_bytes=os.path.getsize(fixture), hard_memory_limit_bytes=a.limit_mib << 20,
                      peak_process_committed_bytes=measured.PeakProcessMemoryUsed, rows=report['rows'],
                      invalid_rows=report['invalid_rows'], elapsed_seconds=round(time.time() - started, 3),
                      complete=True, exit_code=exit_code.value, platform=sys.platform)
        with open(prefix + '-measurement.json', 'w', encoding='utf-8') as f: json.dump(result, f, indent=2)
        print(json.dumps(result, indent=2))
    finally:
        if pi.hProcess:
            k.TerminateProcess(pi.hProcess, 1); k.CloseHandle(pi.hProcess)
        if pi.hThread: k.CloseHandle(pi.hThread)
        k.CloseHandle(job)

if __name__ == '__main__': main()
