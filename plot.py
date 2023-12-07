import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
import seaborn as sns
import argparse

parser = argparse.ArgumentParser(description='Plot a bar chart with different colors for each bar.')
parser.add_argument('--palette', type=str, default='viridis', help='Specify the seaborn color palette (e.g., Set1, Set2, viridis, etc.)')
parser.add_argument('--file', type=str, default='results_doubling.out', help='Specify the csv file to plot')

args = parser.parse_args()

# Read in the saved CSV data.
benchmark_data = pd.read_csv(args.file, header=0, names=['name', 'time', 'range'])

# Use the name of the benchmark to extract the number of worker threads used.
# e.g. "Filter/16-8" used 16 worker threads (goroutines).
# Note how the benchmark name corresponds to the regular expression 'Filter/\d+_workers-\d+'.
# Also note how we place brackets around the value we want to extract.
benchmark_data['threads'] = benchmark_data['name'].str.extract('ParallelGameOfLife/(\d+)_workers-\d+').apply(pd.to_numeric)
benchmark_data['cpu_cores'] = benchmark_data['name'].str.extract('ParallelGameOfLife/\d+_workers-(\d+)').apply(pd.to_numeric)

# Plot a bar chart with different colors for each bar.
ax = sns.barplot(data=benchmark_data, x='threads', y='time', hue='threads', palette=sns.color_palette(args.palette, n_colors=len(benchmark_data)), legend=False)

# Set descriptive axis labels.
ax.set(xlabel='Worker threads used', ylabel='Time taken (s)')

# Display the full figure.
plt.show()
