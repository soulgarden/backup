import os
import signal
import subprocess


class Cancelled(Exception):
    pass


class Processes:
    def __init__(self, grace):
        self.grace = grace
        self.child = None
        self.cancelled = False

    def handle_signal(self, signum, frame):
        self.cancelled = True
        signal.signal(signal.SIGALRM, lambda *_: os._exit(143))
        signal.alarm(self.grace)
        self.stop()
        raise Cancelled()

    def stop(self):
        if self.child is None or self.child.poll() is not None:
            return
        os.killpg(self.child.pid, signal.SIGTERM)
        try:
            self.child.wait(timeout=max(1, self.grace // 2))
        except subprocess.TimeoutExpired:
            os.killpg(self.child.pid, signal.SIGKILL)
            self.child.wait()

    def run(self, args, env=None, output=None):
        if self.cancelled:
            raise Cancelled()
        # Never print server/SDK diagnostics which may contain credentials.
        with open(os.devnull, "wb") as devnull:
            self.child = subprocess.Popen(args, env=env, start_new_session=True,
                                          stdin=devnull, stdout=output or subprocess.PIPE,
                                          stderr=devnull)
            try:
                data, _ = self.child.communicate()
                if self.child.returncode:
                    raise RuntimeError("PROCESS_FAILED")
                return data.decode() if data else ""
            except BaseException:
                self.stop()
                raise
            finally:
                self.child = None
