module JadFn
  NAME = /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/

  # The method fn.<name>(args) calls: call, else the one named like the file (a dash written _),
  # else the constant's only singleton method.
  def self.entry(constant, name)
    methods = constant.singleton_methods
    [:call, name.tr("-", "_").to_sym].find { |candidate| methods.include?(candidate) } || (methods.size == 1 ? methods.first : nil)
  end

  class Library
    def method_missing(name, *args, **options, &block)
      name = name.to_s
      dir = ENV["JAD_RUNNER_DIR"]
      raise "fn.#{name} is not a helper name: use letters, digits, _ and - and start with a letter or digit" unless NAME.match?(name)
      file = dir && File.join(dir, name + ".rb")
      raise "no helper fn.#{name} in #{dir || '(the helper library does not exist)'}" unless file && File.file?(file)
      require file
      constant_name = name.split(/[_-]/).map(&:capitalize).join
      raise "helper #{file} defines no constant #{constant_name}" unless Object.const_defined?(constant_name)
      constant = Object.const_get(constant_name)
      return constant if args.empty? && options.empty? && block.nil?
      entry = JadFn.entry(constant, name)
      unless entry
        raise "fn.#{name} has no single entry to call (#{constant_name} has #{constant.singleton_methods.empty? ? 'no singleton methods' : constant.singleton_methods.join(', ')}): define self.call or self.#{name.tr('-', '_')}"
      end
      constant.public_send(entry, *args, **options, &block)
    end

    def respond_to_missing?(*) = true
  end

  LIBRARY = Library.new
end

def fn = JadFn::LIBRARY
