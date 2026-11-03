<?php
// exec's fn for php, run before the program by auto_prepend_file. $fn->name(...) calls the saved
// helper functions_dir/php/<name>.php ($fn->{'my-help'}(...) for a dash); $fn->name is the helper
// itself. A helper file returns the helper: a callable (its entry), or an array of callables whose
// entry is 'call', else the one named like the file (a dash written _), else its only callable.
// Inside a function, write global $fn; first.
final class JadFn
{
    private array $helpers = [];

    public function __get(string $name): mixed
    {
        if (array_key_exists($name, $this->helpers)) {
            return $this->helpers[$name];
        }
        if (!preg_match('/\A[A-Za-z0-9][A-Za-z0-9_-]*\z/', $name)) {
            throw new Error("\$fn->$name is not a helper name: use letters, digits, _ and - and start with a letter or digit");
        }
        $dir = getenv('JAD_RUNNER_DIR');
        $file = $dir === false ? null : "$dir/$name.php";
        if ($file === null || !is_file($file)) {
            throw new Error("no helper \$fn->$name in " . ($dir === false ? '(the helper library does not exist)' : $dir));
        }
        $helper = (static fn (string $path) => require $path)($file);
        if (!is_callable($helper) && !is_array($helper)) {
            throw new Error("helper $file must return a callable or an array of callables");
        }
        return $this->helpers[$name] = $helper;
    }

    public function __call(string $name, array $args): mixed
    {
        $helper = $this->__get($name);
        if (is_callable($helper)) {
            return $helper(...$args);
        }
        foreach (['call', str_replace('-', '_', $name)] as $key) {
            if (isset($helper[$key]) && is_callable($helper[$key])) {
                return $helper[$key](...$args);
            }
        }
        $callables = array_keys(array_filter($helper, 'is_callable'));
        if (count($callables) === 1) {
            return $helper[$callables[0]](...$args);
        }
        throw new Error("\$fn->$name has no single entry to call ("
            . ($callables ? 'it has ' . implode(', ', $callables) . '; call $fn->' . $name . "['" . $callables[0] . "'](...)" : 'it has no callable')
            . "): add 'call' or '" . str_replace('-', '_', $name) . "'");
    }
}

$fn = new JadFn();
