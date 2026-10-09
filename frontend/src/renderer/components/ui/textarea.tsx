import * as React from "react";
import { cn } from "../../lib/utils";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(
	({ className, ...props }, ref) => (
		<textarea
			data-slot="textarea"
			className={cn(
				"min-h-24 w-full min-w-0 resize-y rounded-md border border-transparent bg-input/50 px-3 py-2 text-sm text-foreground transition-[color,box-shadow,background-color] outline-none placeholder:text-muted-foreground focus-visible:outline-none disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive dark:aria-invalid:border-destructive/50",
				className,
			)}
			ref={ref}
			{...props}
		/>
	),
);

Textarea.displayName = "Textarea";
