import builtins as _builtins
import importlib as _importlib
import importlib.machinery as _machinery
import importlib.util as _util
import os as _os
import sys as _sys
import types as _types

# Every interpreter start loads this file, so it imports only what start-up needs: inspect and
# re cost about 12 ms of a 15 ms load. A function is a types.FunctionType (what
# inspect.isfunction tests), and re is imported when code first reads fn.<name>.

_here = _os.path.dirname(_os.path.abspath(__file__))


def _entry(module):
    # The function named like the module (a dash written _), else its only public function.
    candidate = getattr(module, module.__name__.replace("-", "_"), None)
    if isinstance(candidate, _types.FunctionType):
        return candidate
    functions = [
        value for key, value in vars(module).items()
        if not key.startswith("_") and isinstance(value, _types.FunctionType) and value.__module__ == module.__name__
    ]
    return functions[0] if len(functions) == 1 else None


class _Helper(_types.ModuleType):
    def __call__(self, *args, **kwargs):
        entry = _entry(self)
        if entry is None:
            names = [k for k, v in vars(self).items() if not k.startswith("_") and isinstance(v, _types.FunctionType) and v.__module__ == self.__name__]
            raise TypeError(
                "fn." + self.__name__ + " has no single entry to call ("
                + ("it defines " + ", ".join(names) + "; call fn." + self.__name__ + "." + names[0] + "(...)" if names else "it defines no function")
                + "): define a function named " + self.__name__.replace("-", "_")
            )
        return entry(*args, **kwargs)


def _callable(module):
    if type(module) is _types.ModuleType:
        module.__class__ = _Helper
    return module


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
        import re as _re

        directory = _os.environ.get("JAD_RUNNER_DIR")
        if not _re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]*", name):
            raise AttributeError("fn." + name + " is not a helper name: use letters, digits, _ and - (a dash needs getattr(fn, 'my-helper')) and start with a letter or digit")
        if not directory:
            raise AttributeError("no helper fn." + name + " in (the helper library does not exist)")
        # A helper the program just wrote must be found, in a folder it may just have made:
        # forget what is cached for this folder, as importlib.invalidate_caches() does for every
        # path (a None entry, kept for a folder that did not exist, is dropped; a finder forgets
        # its listing). That call imports importlib.metadata in 3.10+, about 20 ms.
        finder = _sys.path_importer_cache.get(directory)
        if finder is None:
            _sys.path_importer_cache.pop(directory, None)
        else:
            finder.invalidate_caches()
        spec = _machinery.PathFinder.find_spec(name, [directory])
        if spec is None:
            raise AttributeError("no helper fn." + name + " in " + directory)
        if directory not in _sys.path:
            _sys.path.insert(0, directory)
        loaded = _sys.modules.get(name)
        if loaded is None:
            return _callable(_importlib.import_module(name))
        if getattr(getattr(loaded, "__spec__", None), "origin", None) == spec.origin:
            return _callable(loaded)
        module = _util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return _callable(module)


_builtins.fn = _Fn()
_chain()
