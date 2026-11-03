# exec's standby shim: it stands in for the code of ruby -e, reads the call's code from fd 3 and
# evaluates it at the top level as -e. Code that does not compile is declined with F on fd 4, so
# a fresh ruby -e reports the error; R on fd 4 means the code runs here.
eval(proc do
  input = IO.for_fd(3, autoclose: true)
  source = input.read
  input.close
  control = IO.for_fd(4, "w", autoclose: true)
  exit!(0) if source.empty?
  begin
    RubyVM::InstructionSequence.compile(source, "-e", "-e", 1)
  rescue ScriptError, StandardError
    control.syswrite("F")
    exit!(0)
  end
  control.syswrite("R")
  control.close
  # ruby -e highlights the failing call from its source; eval keeps the source for that.
  RubyVM.keep_script_lines = true
  # Registered first, so it runs after the program's own at_exit handlers, as the report of
  # ruby -e does: the error is printed without the shim's frames.
  at_exit do
    error = $!
    next if error.nil? || error.is_a?(SystemExit) || error.is_a?(SignalException)
    lines = error.full_message(highlight: false, order: :top).lines
    cut = lines.rindex { |line| line.match?(/in [`'](Kernel#)?eval'/) }
    $stdout.flush
    $stderr.write(cut && cut > 0 ? lines[0...cut].join : lines.join)
    $stderr.flush
    exit!(1)
  end
  source
end.call, TOPLEVEL_BINDING, "-e", 1)
