def _jad_standby():
    # exec's standby shim: it stands in for the code of python -c, reads the call's code from
    # fd 3 and runs it as python -c would. Code it cannot compile is declined with F on fd 4, so
    # a fresh interpreter reports the error; R on fd 4 means the code runs here.
    import os, sys
    with open(3, "rb", closefd=True) as stream:
        source = stream.read()
    control = open(4, "wb", buffering=0, closefd=True)
    if not source:
        os._exit(0)
    text = source.decode("utf-8", "surrogateescape")
    try:
        code = compile(text, "<string>", "exec", dont_inherit=True)
    except BaseException:
        control.write(b"F")
        os._exit(0)
    control.write(b"R")
    control.close()
    # python -c keeps its source for tracebacks (3.13); so does this.
    register = getattr(sys.modules.get("linecache"), "_register_code", None)
    if register is not None:
        register(code, text, "<string>")
    shim = sys._getframe(1).f_code
    hook = sys.excepthook

    def excepthook(kind, value, trace):
        # The shim's own frame is not the program's: an uncaught error prints as with python -c.
        if trace is not None and trace.tb_frame.f_code is shim:
            trace = trace.tb_next
        if value is not None and value.__traceback__ is not None and value.__traceback__.tb_frame.f_code is shim:
            value.__traceback__ = value.__traceback__.tb_next
        return hook(kind, value, trace)

    sys.excepthook = excepthook
    del globals()["_jad_standby"]
    return code
exec(_jad_standby())
