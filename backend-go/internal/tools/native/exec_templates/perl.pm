# exec's fn for perl, loaded as JadFn by perl -MJadFn from a per-call folder in front of PERL5LIB.
# fn->name(...) calls the saved helper functions_dir/perl/<name>.pl; fn::call('my-help', ...) calls
# one whose name is not a method name; fn::lib('name') returns the helper itself. A helper file's
# last value is the helper: a code ref (its entry), or a hash ref of code refs whose entry is
# call, else the one named like the file (a dash written _), else its only code ref.
package JadFn;
1;

package fn;
# Not warnings: it costs about 1.3 ms of every perl start, and it would cover this file only.
use strict;

my %helpers;

sub lib {
    my ($name) = @_;
    return $helpers{$name} if exists $helpers{$name};
    die "fn->$name is not a helper name: use letters, digits, _ and - and start with a letter or digit\n"
        unless defined $name && $name =~ /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/;
    my $dir = $ENV{JAD_RUNNER_DIR};
    my $file = defined $dir ? "$dir/$name.pl" : undef;
    die "no helper fn->$name in " . ($dir // "(the helper library does not exist)") . "\n"
        unless defined $file && -f $file;
    my $helper = do $file;
    die $@ if $@;
    die "helper $file: $!\n" if !defined $helper && $!;
    die "helper $file must end with a code ref or a hash ref of code refs\n"
        unless ref $helper eq 'CODE' || ref $helper eq 'HASH';
    return $helpers{$name} = $helper;
}

sub entry {
    my ($name, $helper) = @_;
    return $helper if ref $helper eq 'CODE';
    (my $sub = $name) =~ tr/-/_/;
    for my $key ('call', $sub) {
        return $helper->{$key} if ref $helper->{$key} eq 'CODE';
    }
    my @code = sort grep { ref $helper->{$_} eq 'CODE' } keys %$helper;
    return $helper->{ $code[0] } if @code == 1;
    die "fn->$name has no single entry to call ("
        . (@code ? "it has " . join(', ', @code) . "; call fn::lib('$name')->{$code[0]}->(...)" : "it has no code ref")
        . "): add call or $sub\n";
}

sub call {
    my ($name, @args) = @_;
    return entry($name, lib($name))->(@args);
}

our $AUTOLOAD;

sub AUTOLOAD {
    my ($class, @args) = @_;
    (my $name = $AUTOLOAD) =~ s/.*:://;
    return if $name eq 'DESTROY';
    return call($name, @args);
}

1;
