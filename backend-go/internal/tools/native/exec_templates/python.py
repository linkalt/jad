import builtins as _builtins
import importlib as _importlib
import importlib.machinery as _machinery
import importlib.util as _util
import os as _os
import re as _re
import sys as _sys

_here = _os.path.dirname(_os.path.abspath(__file__))


def _chain():
    # Run the sitecustomize this one shadows (a venv's or the user's), if any.
    others = [p for p in _sys.path if _os.path.abspath(p or ".") != _here]
    spec = _machinery.PathFinder.find_spec("sitecustomize", others)
    if spec is not None and spec.loader is not None:
        spec.loader.exec_module(_util.module_from_spec(spec))


class _Fn:
    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)
        directory = _os.environ.get("JAD_RUNNER_DIR")
        if not _re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]*", name):
            raise AttributeError("fn." + name + " is not a helper name: use letters, digits, _ and - (a dash needs getattr(fn, 'my-helper')) and start with a letter or digit")
        if not directory:
            raise AttributeError("no helper fn." + name + " in (the helper library does not exist)")
        _importlib.invalidate_caches()
        spec = _machinery.PathFinder.find_spec(name, [directory])
        if spec is None:
            raise AttributeError("no helper fn." + name + " in " + directory)
        if directory not in _sys.path:
            _sys.path.insert(0, directory)
        loaded = _sys.modules.get(name)
        if loaded is None:
            return _importlib.import_module(name)
        if getattr(getattr(loaded, "__spec__", None), "origin", None) == spec.origin:
            return loaded
        module = _util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module


_builtins.fn = _Fn()
_chain()
