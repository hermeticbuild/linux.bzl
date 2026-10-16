/* SPDX-License-Identifier: GPL-3.0-or-later
 * Capture operations from GNU Make's expansion parser without running them.
 */
FILE *make_ast_expression_output;
static int make_ast_expr_first;

static void
make_ast_expr_string (const char *value, size_t length)
{
  size_t i;
  FILE *out = make_ast_expression_output;
  fputc ('"', out);
  for (i = 0; i < length; ++i)
    {
      unsigned char c = value[i];
      if (c == '"' || c == '\\')
        { fputc ('\\', out); fputc (c, out); }
      else if (c < 32) fprintf (out, "\\u%04x", c);
      else fputc (c, out);
    }
  fputc ('"', out);
}

static void
make_ast_expr_begin (const char *op)
{
  if (!make_ast_expr_first) fputc (',', make_ast_expression_output);
  make_ast_expr_first = 0;
  fputs ("{\"op\":", make_ast_expression_output);
  make_ast_expr_string (op, strlen (op));
}

void
make_ast_expr_literal (const char *value, size_t length)
{
  if (!length) return;
  make_ast_expr_begin ("literal");
  fputs (",\"value\":", make_ast_expression_output);
  make_ast_expr_string (value, length);
  fputc ('}', make_ast_expression_output);
}

void
make_ast_export_expression (FILE *out, const char *value, size_t length)
{
  FILE *saved_output = make_ast_expression_output;
  int saved_first = make_ast_expr_first;
  char *saved_buffer;
  size_t saved_length;
  install_variable_buffer (&saved_buffer, &saved_length);
  make_ast_expression_output = out;
  make_ast_expr_first = 1;
  fputc ('[', out);
  variable_expand_string (NULL, value, length);
  fputc (']', out);
  restore_variable_buffer (saved_buffer, saved_length);
  make_ast_expression_output = saved_output;
  make_ast_expr_first = saved_first;
}

static void
make_ast_expr_reference (const char *name, size_t length)
{
  struct variable *variable;
  int plain = 1;
  size_t i;
  for (i = 0; i < length; ++i)
    if (!(isalnum ((unsigned char)name[i]) || name[i] == '_'))
      { plain = 0; break; }
  if (plain && length > 7 && strneq (name, "CONFIG_", 7))
    {
      make_ast_expr_begin ("config");
      fputs (",\"name\":", make_ast_expression_output);
      make_ast_expr_string (name, length);
    }
  else if (plain && (variable = lookup_variable (name, length))
           && variable->origin == o_command && !strchr (variable->value, '$'))
    {
      make_ast_expr_literal (variable->value, strlen (variable->value));
      return;
    }
  else
    {
      make_ast_expr_begin ("reference");
      fputs (",\"name\":", make_ast_expression_output);
      make_ast_export_expression (make_ast_expression_output, name, length);
    }
  fputc ('}', make_ast_expression_output);
}

void
make_ast_expr_function (const char *name, unsigned int count, char **arguments,
                        int expand_arguments)
{
  unsigned int i;
  make_ast_expr_begin ("function");
  fputs (",\"name\":", make_ast_expression_output);
  make_ast_expr_string (name, strlen (name));
  fprintf (make_ast_expression_output, ",\"expand_arguments\":%s,\"arguments\":[",
           expand_arguments ? "true" : "false");
  for (i = 0; i < count; ++i)
    {
      if (i) fputc (',', make_ast_expression_output);
      make_ast_export_expression (make_ast_expression_output, arguments[i], SIZE_MAX);
    }
  fputs ("]}", make_ast_expression_output);
}
