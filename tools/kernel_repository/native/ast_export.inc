static int ast_condition_mode;
/* SPDX-License-Identifier: GPL-3.0-or-later
 * Prototype syntax export for GNU Make 4.4.1. Included by src/read.c.
 * Reuses Make's readers and lexers; never expands source expressions.
 */
static FILE *ast_output;

static void
ast_string_n (const char *value, size_t length)
{
  size_t i;
  fputc ('"', ast_output);
  for (i = 0; i < length; ++i)
    {
      unsigned char c = value[i];
      if (c == '"' || c == '\\')
        { fputc ('\\', ast_output); fputc (c, ast_output); }
      else if (c < 32)
        fprintf (ast_output, "\\u%04x", c);
      else
        fputc (c, ast_output);
    }
  fputc ('"', ast_output);
}

static void
ast_field_n (const char *name, const char *value, size_t length)
{
  fputc (',', ast_output);
  ast_string_n (name, strlen (name));
  fputc (':', ast_output);
  ast_string_n (value, length);
}

static void
ast_field (const char *name, const char *value)
{
  ast_field_n (name, value, strlen (value));
}

static void
ast_begin (const char *kind, const floc *location)
{
  if (!ast_output)
    {
      ast_output = fopen (getenv ("LINUX_BZL_MAKE_AST"), "w");
      if (!ast_output)
        pfatal_with_name (getenv ("LINUX_BZL_MAKE_AST"));
    }
  fputs ("{\"kind\":", ast_output);
  ast_string_n (kind, strlen (kind));
  ast_field ("file", location->filenm ? location->filenm : "<eval>");
  fprintf (ast_output, ",\"line\":%lu", location->lineno);
}

static void
ast_end (void)
{
  fputs ("}\n", ast_output);
  if (ferror (ast_output))
    pfatal_with_name ("writing Make syntax export");
}

static const char *
ast_flavor (enum variable_flavor flavor)
{
  switch (flavor)
    {
    case f_simple: return "simple";
    case f_recursive: return "recursive";
    case f_expand: return "expand";
    case f_append: return "append";
    case f_conditional: return "conditional";
    case f_shell: return "shell";
    default: return "unknown";
    }
}

static void
ast_modifiers (const struct vmodifiers *mods)
{
  fprintf (ast_output, ",\"override\":%s,\"private\":%s,\"export\":%d",
           mods->override_v ? "true" : "false",
           mods->private_v ? "true" : "false", (int)mods->export_v);
}

/* Reuse Make's lexer to keep function calls with embedded spaces together.
 * Adjacent lexer fragments (for example foo=bar.o) remain one Make word. */
static void
ast_words (const char *value)
{
  char *buffer = xstrdup (value), *cursor = buffer, *word;
  char *start = NULL, *end = NULL;
  size_t length;
  int first = 1;
  fputs (",\"words\":[", ast_output);
  while (get_next_mword (cursor, &word, &length) != w_eol)
    {
      if (start && word > end && ISSPACE (*end))
        {
          if (!first) fputc (',', ast_output);
          ast_string_n (start, end - start);
          first = 0;
          start = NULL;
        }
      if (!start) start = word;
      end = word + length;
      cursor = end;
    }
  if (start)
    {
      if (!first) fputc (',', ast_output);
      ast_string_n (start, end - start);
    }
  fputc (']', ast_output);
  free (buffer);
}

static void
ast_assignment (const char *kind, const char *text, const struct variable *var,
                const struct vmodifiers *mods, const floc *location)
{
  ast_begin (kind, location);
  ast_field ("text", text);
  ast_field_n ("name", var->name, var->length);
  ast_field ("flavor", ast_flavor (var->flavor));
  ast_field ("value", var->value);
  ast_words (var->value);
  if (getenv ("LINUX_BZL_MAKE_EXPRESSIONS"))
    {
      fputs (",\"name_expression\":", ast_output);
      make_ast_export_expression (ast_output, var->name, var->length);
      fputs (",\"value_expression\":", ast_output);
      make_ast_export_expression (ast_output, var->value, SIZE_MAX);
    }
  ast_modifiers (mods);
  ast_end ();
}

/* Parse without evaluating: retain both branches, includes, evals, and recipes.
 * Conditional arguments and Make expressions deliberately remain raw strings.
 */
static void
ast_read (struct ebuffer *ebuf)
{
  long nlines = 0;
  int have_targets = 0;
  char saved_prefix = cmd_prefix;
  cmd_prefix = '\t';
  ast_begin ("file_begin", &ebuf->floc);
  ast_end ();
  while (1)
    {
      char *line, *p, *body, *word;
      size_t length;
      struct variable var;
      struct vmodifiers mods;
      enum make_word_type type;

      ebuf->floc.lineno += nlines;
      nlines = readline (ebuf);
      if (nlines < 0)
        break;
      line = ebuf->buffer;
      if (ebuf->floc.lineno == 1 && strlen (line) >= 3
          && (unsigned char)line[0] == 0xef
          && (unsigned char)line[1] == 0xbb
          && (unsigned char)line[2] == 0xbf)
        line += 3;
      /* As in eval(), a prefix starts a recipe only after a rule.
       * Indented assignments outside a rule are ordinary Make statements. */
      if (line[0] == cmd_prefix && have_targets)
        {
          ast_begin ("recipe", &ebuf->floc);
          ast_field ("text", line);
          ast_field ("value", line + 1);
          ast_end ();
          continue;
        }
      collapse_continuations (line);
      remove_comments (line);
      p = next_token (line);
      if (!*p)
        continue;
      body = parse_var_assignment (p, 0, &mods);
      if (mods.assign_v)
        {
          have_targets = 0;
          if (mods.define_v)
            do_define (body, o_file, ebuf, &mods);
          else if (mods.undefine_v)
            {
              ast_begin ("undefine", &ebuf->floc);
              ast_field ("text", p);
              ast_field ("name", body);
              ast_modifiers (&mods);
              ast_end ();
            }
          else
            {
              parse_variable_definition (body, &var);
              ast_assignment ("assignment", p, &var, &mods, &ebuf->floc);
              if (var.length == 13 && strneq (var.name, ".RECIPEPREFIX", 13))
                {
                  if (strchr (var.value, '$'))
                    O (fatal, &ebuf->floc, _("AST export needs a literal .RECIPEPREFIX"));
                  cmd_prefix = var.value[0] ? var.value[0] : '\t';
                }
            }
          continue;
        }
      word = end_of_token (p);
      length = word - p;
      body = next_token (word);
#define AST_WORD(s) (length == CSTRLEN(s) && strneq(p, s, CSTRLEN(s)))
      if (AST_WORD("ifdef") || AST_WORD("ifndef") || AST_WORD("ifeq")
          || AST_WORD("ifneq") || AST_WORD("else") || AST_WORD("endif")
          || AST_WORD("include") || AST_WORD("-include") || AST_WORD("sinclude")
          || AST_WORD("export") || AST_WORD("unexport") || AST_WORD("vpath")
          || AST_WORD("load") || AST_WORD("-load"))
        {
          if (!(AST_WORD("ifdef") || AST_WORD("ifndef") || AST_WORD("ifeq")
                || AST_WORD("ifneq") || AST_WORD("else") || AST_WORD("endif")))
            have_targets = 0;
          ast_begin ("directive", &ebuf->floc);
          ast_field ("text", p);
          ast_field_n ("directive", p, length);
          ast_field ("arguments", body);
          if (getenv ("LINUX_BZL_MAKE_EXPRESSIONS"))
            {
              char *copy = xstrdup (p);
              ast_condition_mode = 1;
              conditional_line (copy, length, &ebuf->floc);
              ast_condition_mode = 0;
              free (copy);
            }
          ast_end ();
          continue;
        }
#undef AST_WORD
      /* Native rule lexer locates separators while skipping variable refs. */
      body = p;
      while (1)
        {
          type = get_next_mword (body, &word, &length);
          if (type == w_eol || type == w_semicolon || type == w_varassign)
            break;
          if (type == w_colon || type == w_dcolon
              || type == w_ampcolon || type == w_ampdcolon)
            {
              char *tail = next_token (word + length);
              char *assignment = parse_var_assignment (tail, 1, &mods);
              if (mods.assign_v && parse_variable_definition (assignment, &var))
                {
                  have_targets = 0;
                  ast_begin ("target_assignment", &ebuf->floc);
                  ast_field ("text", p);
                  ast_field_n ("targets", p, word - p);
                  ast_field_n ("separator", word, length);
                  ast_field_n ("name", var.name, var.length);
                  ast_field ("flavor", ast_flavor(var.flavor));
                  ast_field ("value", var.value);
                  ast_modifiers (&mods);
                  ast_end ();
                }
              else
                {
                  have_targets = 1;
                  ast_begin ("rule", &ebuf->floc);
                  ast_field ("text", p);
                  ast_field_n ("targets", p, word - p);
                  ast_field_n ("separator", word, length);
                  ast_field ("tail", tail);
                  if (getenv ("LINUX_BZL_MAKE_EXPRESSIONS"))
                    {
                      fputs (",\"targets_expression\":", ast_output);
                      make_ast_export_expression (ast_output, p, word - p);
                      fputs (",\"tail_expression\":", ast_output);
                      make_ast_export_expression (ast_output, tail, SIZE_MAX);
                    }
                  ast_end ();
                }
              break;
            }
          body = word + length;
        }
      if (type == w_eol || type == w_semicolon || type == w_varassign)
        {
          have_targets = 0;
          ast_begin ("expression", &ebuf->floc);
          ast_field ("text", p);
          ast_end ();
        }
    }
  ast_begin ("file_end", &ebuf->floc);
  ast_end ();
  cmd_prefix = saved_prefix;
}
