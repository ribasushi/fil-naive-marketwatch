#!/usr/bin/perl

use warnings;
use strict;

use DBIx::Class::Schema::Loader;
use DBIx::Class::_Util 'sigwarn_silencer';
use File::Basename 'dirname';
use SQL::Translator;

{
  package GraphedSchema;
  use base 'DBIx::Class::Schema::Loader';

  __PACKAGE__->loader_options (
    naming => 'v8',
    db_schema => 'naive',
    qualify_objects => 1,
    exclude => qr/^(?: storage_stats )/x,
  );
}

$SIG{__WARN__} = sigwarn_silencer(qr/collides with an inherited method/);

{
  no warnings 'redefine';
  *SQL::Translator::Schema::add_view = sub {
    my $s = shift;
    my %args = @_;
    my $t = $s->add_table(%args);
    $t->add_field(
      name => $_,
      size => 0,
      is_auto_increment => 0,
      is_foreign_key => 0,
      is_nullable => 0,
    ) for @{$args{fields}};
    return $t;
  };
}

my $schema = GraphedSchema->connect('dbi:Pg:service=naive');
$schema->storage->ensure_connected;
delete $schema->source('Provider')->{_relationships}{$_} for qw( providers_info ); # bugs, bugs everywhere :(


use Devel::Dwarn;

my $views = [qw( )];

my $trans = SQL::Translator->new(
    parser        => 'SQL::Translator::Parser::DBIx::Class',
    parser_args   => { dbic_schema => $schema },
    producer      => 'GraphViz',
    producer_args => {
        width => 0,
        height => 0,
        output_type      => 'svg',
        out_file         => dirname(__FILE__) . '/pg_schema_diagram.svg',
        show_constraints => 1,
        show_datatypes   => 1,
        show_indexes     => 0, # this doesn't actually work on the loader side
        show_sizes       => 1,
        friendly_ints    => 1,
        cluster => [
           { name => "views", tables => $views },
        ],
    },
) or die SQL::Translator->error;
$trans->translate or die $trans->error;
