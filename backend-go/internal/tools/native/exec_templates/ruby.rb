module JadFn
  NAME = /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/

  class Library
    def method_missing(name, *_args)
      name = name.to_s
      dir = ENV["JAD_RUNNER_DIR"]
      raise "fn.#{name} is not a helper name: use letters, digits, _ and - and start with a letter or digit" unless NAME.match?(name)
      file = dir && File.join(dir, name + ".rb")
      raise "no helper fn.#{name} in #{dir || '(the helper library does not exist)'}" unless file && File.file?(file)
      require file
      constant = name.split(/[_-]/).map(&:capitalize).join
      raise "helper #{file} defines no constant #{constant}" unless Object.const_defined?(constant)
      Object.const_get(constant)
    end

    def respond_to_missing?(*) = true
  end

  LIBRARY = Library.new
end

def fn = JadFn::LIBRARY
